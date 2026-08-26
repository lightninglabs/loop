package loopin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	expiryRecoveryHeight      = 2_200
	expiryRecoveryLookupError = "lookup error"
)

// TestExpiredLoopInRecovery retains the real deposit FSM while lnd's historical
// HTLC notification is delayed past startup and a current-tip notification.
func TestExpiredLoopInRecovery(t *testing.T) {
	for _, state := range []fsm.StateType{
		deposit.LoopingIn, deposit.SweepHtlcTimeout,
	} {
		t.Run(string(state), func(t *testing.T) {
			h := newExpiryRecoveryHarness(t, state, false)
			h.lnd.SetInvoice(&lndclient.Invoice{
				Hash: h.f.loopIn.SwapHash, State: invoices.ContractCanceled,
			})
			checker := &expiryRecoveryChecker{
				calls: make(chan struct{}, 4),
			}
			h.f.cfg.TxOutChecker = checker
			result, conf, blocks := h.monitor(t)

			// lnd initially reports the current tip before finishing the
			// historical HTLC lookup. Its missing deposit must stay owned.
			h.sendBlock(t, blocks, expiryRecoveryHeight)
			expiryRecoveryReceive(t, h.ctx, checker.calls)
			h.sendBlock(t, blocks, expiryRecoveryHeight)
			require.NoError(t, h.manager.EnsureDepositsFresh(h.ctx))
			h.requireActive(t, state)

			// Now deliver the old HTLC confirmation. A second current tip
			// must be able to transition the still-active deposit to its
			// HTLC refund path instead of losing its FSM in reconciliation.
			fundingTx, err := h.f.loopIn.createHtlcTx(
				h.lnd.ChainParams, h.f.loopIn.HtlcTxFeeRate, 1,
			)
			require.NoError(t, err)
			h.sendConfirmation(t, conf, fundingTx, 1_500)
			h.sendBlock(t, blocks, expiryRecoveryHeight)
			require.Equal(t, OnSweepHtlcTimeout,
				expiryRecoveryReceive(t, h.ctx, result))
			h.requireActive(t, deposit.SweepHtlcTimeout)

			require.Equal(t, OnHtlcTimeoutSweepPublished,
				h.f.SweepHtlcTimeoutAction(h.ctx, nil))
			refund := expiryRecoveryReceive(t, h.ctx, h.wallet.published)
			require.Equal(t, wire.OutPoint{
				Hash: fundingTx.TxHash(), Index: 0,
			}, refund.TxIn[0].PreviousOutPoint)

			finished := make(chan fsm.EventType, 1)
			go func() {
				finished <- h.f.MonitorHtlcTimeoutSweepAction(h.ctx, nil)
			}()
			refundConf := expiryRecoveryReceive(t, h.ctx, h.notifier.confs)
			h.sendConfirmation(t, refundConf, refund, expiryRecoveryHeight+3)
			require.Equal(t, OnHtlcTimeoutSwept,
				expiryRecoveryReceive(t, h.ctx, finished))
			h.requireStoredState(t, deposit.HtlcTimeoutSwept)
		})
	}
}

// TestExpiredLoopInPaymentFailure keeps ownership after a deadline or recovered
// rejection when availability is unknown, then accepts a delayed historical HTLC.
func TestExpiredLoopInPaymentFailure(t *testing.T) {
	for _, failure := range []string{"deadline", "recovered rejection"} {
		for _, availability := range []string{
			"missing", expiryRecoveryLookupError, "no checker",
			"empty original snapshot", "malformed original outpoint",
		} {
			t.Run(failure+"/"+availability, func(t *testing.T) {
				h := newExpiryRecoveryHarness(t, deposit.LoopingIn, false)
				h.f.loopIn.PaymentTimeoutSeconds = 1
				h.f.loopIn.ConfirmationRiskDecisionTime = time.Now().Add(-time.Hour)
				if failure == "recovered rejection" {
					h.f.loopIn.ConfirmationRiskDecision =
						ConfirmationRiskDecisionRejected
				}
				checker := &expiryRecoveryChecker{
					calls: make(chan struct{}, 4),
				}
				if availability == expiryRecoveryLookupError {
					checker.err = errors.New("backend unavailable")
				}
				if availability == "no checker" {
					h.f.cfg.TxOutChecker = nil
				} else {
					h.f.cfg.TxOutChecker = checker
				}
				switch availability {
				case "empty original snapshot":
					h.f.loopIn.DepositOutpoints = nil
				case "malformed original outpoint":
					h.f.loopIn.DepositOutpoints = []string{"invalid:outpoint"}
				}
				result, conf, blocks := h.monitor(t)
				expiryRecoveryReceive(t, h.ctx, h.lnd.FailInvoiceChannel)
				if availability == "missing" ||
					availability == expiryRecoveryLookupError {

					expiryRecoveryReceive(t, h.ctx, checker.calls)
				}

				// This unbuffered handoff occurs after the failure handler
				// finishes, so the state check does not race that handler.
				h.sendBlock(t, blocks, expiryRecoveryHeight)
				require.NoError(t, h.manager.EnsureDepositsFresh(h.ctx))
				h.requireActive(t, deposit.LoopingIn)

				// Keeping the same subscriptions alive is essential: a retry
				// loop at recovered rejection would never consume this event.
				fundingTx, err := h.f.loopIn.createHtlcTx(
					h.lnd.ChainParams, h.f.loopIn.HtlcTxFeeRate, 1,
				)
				require.NoError(t, err)
				h.sendConfirmation(t, conf, fundingTx, 1_500)
				h.sendBlock(t, blocks, expiryRecoveryHeight)
				require.Equal(t, OnSweepHtlcTimeout,
					expiryRecoveryReceive(t, h.ctx, result))
				h.requireActive(t, deposit.SweepHtlcTimeout)
			})
		}
	}
}

// TestExpiredLoopInRuntimeRejection keeps a confirmed HTLC owned when a server
// rejection arrives after confirmation instead of returning the unlock event.
func TestExpiredLoopInRuntimeRejection(t *testing.T) {
	h := newExpiryRecoveryHarness(t, deposit.LoopingIn, false)
	notifications := &mockNotificationManager{
		riskRejected: make(
			chan *swapserverrpc.ServerStaticLoopInRiskRejectedNotification, 1,
		),
	}
	h.f.cfg.NotificationManager = notifications
	result, conf, blocks := h.monitor(t)
	fundingTx, err := h.f.loopIn.createHtlcTx(
		h.lnd.ChainParams, h.f.loopIn.HtlcTxFeeRate, 1,
	)
	require.NoError(t, err)
	h.sendConfirmation(t, conf, fundingTx, 1_500)
	notifications.riskRejected <- &swapserverrpc.ServerStaticLoopInRiskRejectedNotification{
		SwapHash: h.f.loopIn.SwapHash[:],
	}
	expiryRecoveryReceive(t, h.ctx, h.lnd.FailInvoiceChannel)
	h.sendBlock(t, blocks, expiryRecoveryHeight)
	require.Equal(t, OnSweepHtlcTimeout,
		expiryRecoveryReceive(t, h.ctx, result))
	h.requireActive(t, deposit.SweepHtlcTimeout)
}

// TestExpiredUnfundedLoopInRecovery releases genuinely unspent inputs and lets
// the ordinary real-manager CSV expiry path refund them.
func TestExpiredUnfundedLoopInRecovery(t *testing.T) {
	h := newExpiryRecoveryHarness(t, deposit.LoopingIn, true)
	h.lnd.SetInvoice(&lndclient.Invoice{
		Hash: h.f.loopIn.SwapHash, State: invoices.ContractCanceled,
	})
	h.f.cfg.TxOutChecker = &expiryRecoveryChecker{
		outputs: map[wire.OutPoint]*wire.TxOut{
			h.dep.OutPoint: {
				Value: int64(h.dep.Value), PkScript: h.dep.AddressParams.PkScript,
			},
		},
	}
	result, _, blocks := h.monitor(t)
	h.sendBlock(t, blocks, expiryRecoveryHeight)
	require.Equal(t, OnSwapTimedOut,
		expiryRecoveryReceive(t, h.ctx, result))

	// Only the loop-in owner releases the inputs. The next real manager
	// block can now legitimately start the deposit's standalone CSV sweep.
	h.sendBlock(t, h.depositBlocks, expiryRecoveryHeight)
	sweep := expiryRecoveryReceive(t, h.ctx, h.wallet.published)
	require.Equal(t, h.dep.OutPoint, sweep.TxIn[0].PreviousOutPoint)
	require.Equal(t, h.dep.AddressParams.Expiry, sweep.TxIn[0].Sequence)
	spend := expiryRecoveryReceive(t, h.ctx, h.notifier.spends)
	spendHash := sweep.TxHash()
	select {
	case spend.updates <- &chainntnfs.SpendDetail{
		SpentOutPoint: &h.dep.OutPoint, SpendingTx: sweep,
		SpenderTxHash: &spendHash,
	}:
	case <-h.ctx.Done():
		t.Fatal(h.ctx.Err())
	}
	conf := expiryRecoveryReceive(t, h.ctx, h.notifier.confs)
	h.sendConfirmation(t, conf, sweep, expiryRecoveryHeight+3)
	expiryRecoveryReceive(t, h.ctx, h.store.updates)
	for {
		stored, err := h.store.GetDeposit(h.ctx, h.dep.ID)
		require.NoError(t, err)
		if stored.IsInState(deposit.Expired) {
			break
		}
		expiryRecoveryReceive(t, h.ctx, h.store.updates)
	}
}

// TestExpiredPaidLoopInRecovery gives a settled invoice precedence even when
// startup occurs after deposit expiry and its original UTXO is already spent.
func TestExpiredPaidLoopInRecovery(t *testing.T) {
	h := newExpiryRecoveryHarness(t, deposit.LoopingIn, false)
	h.lnd.SetInvoice(&lndclient.Invoice{
		Hash: h.f.loopIn.SwapHash, State: invoices.ContractSettled,
	})
	result, _, _ := h.monitor(t)
	require.Equal(t, OnPaymentReceived,
		expiryRecoveryReceive(t, h.ctx, result))
	require.Equal(t, OnSucceeded, h.f.PaymentReceivedAction(h.ctx, nil))
	h.requireStoredState(t, deposit.LoopedIn)
}

// expiryRecoveryHarness restarts the real manager from a persisted owned
// deposit at a tip beyond both its CSV maturity and the swap HTLC's expiry.
type expiryRecoveryHarness struct {
	ctx           context.Context
	db            *loopdb.SqliteSwapStore
	lnd           *test.LndMockServices
	manager       *deposit.Manager
	store         *expiryRecoveryStore
	notifier      *expiryRecoveryNotifier
	wallet        *expiryRecoveryWallet
	dep           *deposit.Deposit
	f             *FSM
	depositBlocks chan int32
	monitorResult chan fsm.EventType
}

func newExpiryRecoveryHarness(t *testing.T, state fsm.StateType,
	unspent bool, extraDeposits ...*deposit.Deposit) *expiryRecoveryHarness {

	t.Helper()
	return newExpiryRecoveryHarnessWithFunding(
		t, state, unspent, nil, extraDeposits...,
	)
}

func newExpiryRecoveryHarnessWithFunding(t *testing.T, state fsm.StateType,
	unspent bool, funding *wire.MsgTx,
	extraDeposits ...*deposit.Deposit) *expiryRecoveryHarness {

	t.Helper()
	db := loopdb.NewTestSqliteDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	lnd := test.NewMockLnd()
	id, err := deposit.GetRandomDepositID()
	require.NoError(t, err)
	dep := &deposit.Deposit{
		ID:   id,
		Hash: chainhash.Hash{1}, Index: 2,
		Value: 200_000, ConfirmationHeight: 600,
		TimeOutSweepPkScript: []byte{txscript.OP_TRUE},
		OnchainCost:          deposit.OnchainCost{Status: deposit.OnchainCostKnown},
	}
	_, clientKey := test.CreateKey(101)
	_, serverKey := test.CreateKey(102)
	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, 1_500, clientKey, serverKey,
	)
	require.NoError(t, err)
	pkScript, err := staticAddress.StaticAddressScript()
	require.NoError(t, err)
	dep.AddressParams = &script.Parameters{
		ClientPubkey: clientKey, ServerPubkey: serverKey,
		Expiry: 1_500, PkScript: pkScript,
		ProtocolVersion: version.ProtocolVersion_V0, InitiationHeight: 500,
	}
	if funding != nil {
		funding.TxOut[0] = wire.NewTxOut(int64(dep.Value), pkScript)
		dep.Hash, dep.Index = funding.TxHash(), 0
		dep.ConfirmationHeight = 0
	}
	addressStore := address.NewSqlStore(db.BaseDB)
	require.NoError(t, addressStore.CreateStaticAddress(ctx, dep.AddressParams))
	dep.AddressParams.ID, err = addressStore.GetStaticAddressID(ctx, pkScript)
	require.NoError(t, err)
	dep.SetState(state)
	store := &expiryRecoveryStore{
		Store: deposit.NewSqlStore(db.BaseDB), updates: make(chan struct{}, 20),
	}
	require.NoError(t, store.CreateDeposit(ctx, dep))
	swapHash := lntypes.Hash{7}
	for _, d := range append([]*deposit.Deposit{dep}, extraDeposits...) {
		if d != dep {
			d.ID, err = deposit.GetRandomDepositID()
			require.NoError(t, err)
			d.AddressParams = dep.AddressParams
			d.TimeOutSweepPkScript = dep.TimeOutSweepPkScript
			d.SetState(state)
			require.NoError(t, store.CreateDeposit(ctx, d))
		}
		require.NoError(t, db.BaseDB.MapDepositToSwap(
			ctx, sqlc.MapDepositToSwapParams{
				DepositID: d.ID[:], SwapHash: swapHash[:],
			},
		))
	}
	notifier := &expiryRecoveryNotifier{
		blocks: make(chan chan int32, 4),
		confs:  make(chan *expiryRecoveryConf, 4),
		spends: make(chan *expiryRecoverySpend, 4),
	}
	wallet := &expiryRecoveryWallet{
		WalletKitClient: lnd.WalletKit, published: make(chan *wire.MsgTx, 4),
	}
	client := &expiryRecoveryClient{LightningClient: lnd.Client}
	addresses := &expiryRecoveryAddress{}
	if unspent {
		addresses.utxos = []*lnwallet.Utxo{{
			OutPoint: dep.OutPoint, Value: dep.Value,
			PkScript: dep.AddressParams.PkScript, Confirmations: expiryRecoveryHeight - 600 + 1,
		}}
	}
	signer := &expiryRecoverySigner{}
	manager := deposit.NewManager(&deposit.ManagerConfig{
		LightningClient: client, AddressManager: addresses,
		Store: store, WalletKit: wallet, ChainNotifier: notifier, Signer: signer,
	})
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- manager.Run(ctx, ready) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(testTimeout):
			t.Error("deposit manager did not stop")
		}
	})
	depositBlocks := expiryRecoveryReceive(t, ctx, notifier.blocks)
	expiryRecoveryReceive(t, ctx, ready)
	deposits, ok := manager.AllStringOutpointsActiveDeposits(
		[]string{dep.OutPoint.String()}, fsm.EmptyState,
	)
	require.True(t, ok)
	require.Len(t, deposits, 1)
	dep = deposits[0]

	f, _ := newInvoiceMonitorTestFSM(
		t, ctx, lnd, lntypes.Hash{7}, ConfirmationRiskDecisionAccepted,
		lnd.LndServices.Invoices,
	)
	f.loopIn.Deposits = []*deposit.Deposit{dep}
	f.loopIn.DepositOutpoints = []string{dep.OutPoint.String()}
	for _, d := range extraDeposits {
		active, ok := manager.AllStringOutpointsActiveDeposits(
			[]string{d.OutPoint.String()}, fsm.EmptyState,
		)
		require.True(t, ok)
		f.loopIn.Deposits = append(f.loopIn.Deposits, active[0])
		f.loopIn.DepositOutpoints = append(
			f.loopIn.DepositOutpoints, d.OutPoint.String(),
		)
	}
	// At initiation (600), the 1600 HTLC expiry safely precedes deposit
	// maturity (2100). Restarting at 2200 is a legitimate offline recovery.
	f.loopIn.InitiationHeight = 600
	f.loopIn.HtlcCltvExpiry = 1_600
	f.loopIn.HtlcTxFeeRate = chainfee.FeePerKwFloor
	f.loopIn.HtlcTimeoutSweepAddress, _ = wallet.NextAddr(
		ctx, "", walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	f.cfg.DepositManager = manager
	f.cfg.TxOutChecker = nil
	f.cfg.ChainNotifier = notifier
	f.cfg.LndClient = client
	f.cfg.WalletKit = wallet
	f.cfg.Signer = signer
	f.cfg.MaxStaticAddrHtlcFeePercentage = 1
	lnd.SetInvoice(&lndclient.Invoice{
		Hash: f.loopIn.SwapHash, State: invoices.ContractOpen,
	})
	return &expiryRecoveryHarness{
		ctx: ctx, db: db, lnd: lnd, manager: manager,
		store: store, notifier: notifier, wallet: wallet, dep: dep, f: f,
		depositBlocks: depositBlocks,
	}
}

func (h *expiryRecoveryHarness) monitor(t *testing.T) (
	chan fsm.EventType, *expiryRecoveryConf, chan int32) {

	t.Helper()
	result := make(chan fsm.EventType, 1)
	h.monitorResult = result
	go func() { result <- h.f.MonitorInvoiceAndHtlcTxAction(h.ctx, nil) }()
	conf := expiryRecoveryReceive(t, h.ctx, h.notifier.confs)
	blocks := expiryRecoveryReceive(t, h.ctx, h.notifier.blocks)
	return result, conf, blocks
}

func (h *expiryRecoveryHarness) sendBlock(t *testing.T, blocks chan int32,
	height int32) {

	t.Helper()
	select {
	case blocks <- height:
	case event := <-h.monitorResult:
		t.Fatalf("monitor stopped before block %d: %s", height, event)
	case <-h.ctx.Done():
		t.Fatal(h.ctx.Err())
	}
}

func (h *expiryRecoveryHarness) sendConfirmation(t *testing.T,
	conf *expiryRecoveryConf, tx *wire.MsgTx, height uint32) {

	t.Helper()
	select {
	case conf.updates <- &chainntnfs.TxConfirmation{
		Tx: tx, BlockHeight: height,
	}:
	case event := <-h.monitorResult:
		t.Fatalf("monitor stopped before historical confirmation: %s", event)
	case <-h.ctx.Done():
		t.Fatal(h.ctx.Err())
	}
}

func (h *expiryRecoveryHarness) requireActive(t *testing.T, state fsm.StateType) {
	t.Helper()
	_, ok := h.manager.AllStringOutpointsActiveDeposits(
		[]string{h.dep.OutPoint.String()}, state,
	)
	require.True(t, ok, "owned deposit lost its active FSM or state")
}

func (h *expiryRecoveryHarness) requireStoredState(t *testing.T,
	state fsm.StateType) {

	t.Helper()
	stored, err := h.store.GetDeposit(h.ctx, h.dep.ID)
	require.NoError(t, err)
	require.Equal(t, state, stored.GetState())
}

func expiryRecoveryReceive[T any](t *testing.T, ctx context.Context,
	ch <-chan T) T {

	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for recovery event")
	}
	var zero T
	return zero
}

type expiryRecoveryStore struct {
	deposit.Store

	updates chan struct{}
}

func (s *expiryRecoveryStore) UpdateDeposit(ctx context.Context,
	d *deposit.Deposit) error {

	err := s.Store.UpdateDeposit(ctx, d)
	s.updates <- struct{}{}
	return err
}

type expiryRecoveryAddress struct {
	deposit.AddressManager

	utxos []*lnwallet.Utxo
}

func (a *expiryRecoveryAddress) ListUnspent(context.Context, int32, int32) (
	[]*lnwallet.Utxo, error) {

	return a.utxos, nil
}

type expiryRecoveryClient struct {
	lndclient.LightningClient
}

func (c *expiryRecoveryClient) GetInfo(context.Context) (*lndclient.Info, error) {
	return &lndclient.Info{
		BlockHeight: expiryRecoveryHeight, BestBlockHash: chainhash.Hash{9}, SyncedToChain: true,
	}, nil
}

type expiryRecoveryWallet struct {
	lndclient.WalletKitClient

	published chan *wire.MsgTx
}

func (w *expiryRecoveryWallet) PublishTransaction(_ context.Context,
	tx *wire.MsgTx, _ string) error {

	w.published <- tx.Copy()
	return nil
}

type expiryRecoverySigner struct {
	lndclient.SignerClient
}

func (s *expiryRecoverySigner) SignOutputRaw(_ context.Context, _ *wire.MsgTx,
	descs []*lndclient.SignDescriptor, _ []*wire.TxOut) ([][]byte, error) {

	sigs := make([][]byte, len(descs))
	for i := range sigs {
		sigs[i] = make([]byte, 64)
	}
	return sigs, nil
}

type expiryRecoveryChecker struct {
	outputs map[wire.OutPoint]*wire.TxOut
	err     error
	calls   chan struct{}
}

func (c *expiryRecoveryChecker) GetTxOuts(context.Context, []wire.OutPoint) (
	map[wire.OutPoint]*wire.TxOut, error) {

	if c.calls != nil {
		select {
		case c.calls <- struct{}{}:
		default:
		}
	}
	return c.outputs, c.err
}

type expiryRecoveryConf struct {
	updates chan *chainntnfs.TxConfirmation
}

type expiryRecoverySpend struct {
	updates chan *chainntnfs.SpendDetail
}

type expiryRecoveryNotifier struct {
	lndclient.ChainNotifierClient

	mu      sync.Mutex
	started bool
	blocks  chan chan int32
	confs   chan *expiryRecoveryConf
	spends  chan *expiryRecoverySpend
}

func (n *expiryRecoveryNotifier) RegisterBlockEpochNtfn(context.Context) (
	chan int32, chan error, error) {

	n.mu.Lock()
	defer n.mu.Unlock()
	blocks := make(chan int32)
	if !n.started {
		blocks = make(chan int32, 1)
		blocks <- expiryRecoveryHeight
		n.started = true
	}
	n.blocks <- blocks
	return blocks, make(chan error), nil
}

func (n *expiryRecoveryNotifier) RegisterConfirmationsNtfn(context.Context,
	*chainhash.Hash, []byte, int32, int32,
	...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation, chan error,
	error) {

	conf := &expiryRecoveryConf{updates: make(chan *chainntnfs.TxConfirmation)}
	n.confs <- conf
	return conf.updates, make(chan error), nil
}

func (n *expiryRecoveryNotifier) RegisterSpendNtfn(context.Context,
	*wire.OutPoint, []byte, int32,
	...lndclient.NotifierOption) (chan *chainntnfs.SpendDetail, chan error,
	error) {

	spend := &expiryRecoverySpend{updates: make(chan *chainntnfs.SpendDetail)}
	n.spends <- spend
	return spend.updates, make(chan error), nil
}
