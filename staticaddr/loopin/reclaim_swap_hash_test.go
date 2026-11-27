package loopin

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// monitorTestContext runs the monitor of static loop-ins against a real
// loop-in store.
type monitorTestContext struct {
	t            *testing.T
	ctx          context.Context
	cancel       context.CancelFunc
	mockLnd      *test.LndMockServices
	clientPubKey *btcec.PublicKey
	serverPubKey *btcec.PublicKey
	addr         btcutil.Address
	depositStore *deposit.SqlStore
	swapStore    *SqlStore

	// addressParams is the static address every deposit belongs to.
	addressParams *script.Parameters

	// txOutChecker, if set, is the wallet lookup the monitor uses.
	txOutChecker TxOutChecker
}

// newMonitorTestContext returns a monitor test context with empty stores.
func newMonitorTestContext(t *testing.T) *monitorTestContext {
	// Create the database before the timeout starts, since creating a
	// Postgres database takes a while.
	testDb := loopdb.NewTestDB(t)
	t.Cleanup(func() { testDb.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	t.Cleanup(cancel)

	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	// Every deposit belongs to a persisted static address.
	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, 1_000, clientPubKey, serverPubKey,
	)
	require.NoError(t, err)
	pkScript, err := staticAddress.StaticAddressScript()
	require.NoError(t, err)
	addressParams := &script.Parameters{
		ClientPubkey:    clientPubKey,
		ServerPubkey:    serverPubKey,
		Expiry:          1_000,
		PkScript:        pkScript,
		ProtocolVersion: version.ProtocolVersion_V0,
	}
	addressStore := address.NewSqlStore(testDb.BaseDB)
	require.NoError(
		t, addressStore.CreateStaticAddress(ctx, addressParams),
	)
	addressParams.ID, err = addressStore.GetStaticAddressID(
		ctx, pkScript,
	)
	require.NoError(t, err)

	return &monitorTestContext{
		t:             t,
		addressParams: addressParams,
		ctx:           ctx,
		cancel:        cancel,
		mockLnd:       test.NewMockLnd(),
		clientPubKey:  clientPubKey,
		serverPubKey:  serverPubKey,
		addr:          addr,
		depositStore:  deposit.NewSqlStore(testDb.BaseDB),
		swapStore: NewSqlStore(
			loopdb.NewTypedStore[Querier](testDb),
			clock.NewTestClock(time.Now()),
			&chaincfg.RegressionNetParams,
		),
	}
}

// newDeposit stores a deposit that is looping in.
func (c *monitorTestContext) newDeposit(hash byte) *deposit.Deposit {
	id, err := deposit.GetRandomDepositID()
	require.NoError(c.t, err)

	d := &deposit.Deposit{
		ID:                   id,
		Hash:                 chainhash.Hash{hash},
		Value:                btcutil.Amount(500_000),
		TimeOutSweepPkScript: []byte{0x00, 0x14, 0x01},
		AddressParams:        c.addressParams,
	}
	d.SetState(deposit.LoopingIn)
	require.NoError(c.t, c.depositStore.CreateDeposit(c.ctx, d))

	return d
}

// newLoopIn returns a swap that selected the deposits and monitors its invoice
// and htlc.
func (c *monitorTestContext) newLoopIn(hash byte,
	deposits ...*deposit.Deposit) *StaticAddressLoopIn {

	var outpoints []string
	for _, d := range deposits {
		if d != nil {
			outpoints = append(outpoints, d.OutPoint.String())
		}
	}

	loopIn := &StaticAddressLoopIn{
		SwapHash:                lntypes.Hash{hash},
		SwapPreimage:            lntypes.Preimage{hash},
		SwapInvoice:             testSwapInvoice,
		HtlcCltvExpiry:          2_000,
		InitiationHeight:        uint32(c.mockLnd.Height),
		InitiationTime:          time.Now(),
		ProtocolVersion:         version.ProtocolVersion_V0,
		ClientPubkey:            c.clientPubKey,
		ServerPubkey:            c.serverPubKey,
		HtlcTimeoutSweepAddress: c.addr,
		DepositOutpoints:        outpoints,
		Deposits:                deposits,
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	return loopIn
}

// startMonitor runs the monitor of the swap, whose invoice is in the given
// state, and returns the channel that its result is sent to.
func (c *monitorTestContext) startMonitor(loopIn *StaticAddressLoopIn,
	invoiceState invoices.ContractState, depositMgr DepositManager,
	store StaticAddressLoopInStore) chan fsm.EventType {

	c.mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  loopIn.SwapHash,
		State: invoiceState,
	})

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    c.clientPubKey,
				ServerPubkey:    c.serverPubKey,
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:       c.mockLnd.ChainNotifier,
		DepositManager:      depositMgr,
		InvoicesClient:      c.mockLnd.LndServices.Invoices,
		LndClient:           c.mockLnd.Client,
		ChainParams:         c.mockLnd.ChainParams,
		NotificationManager: &mockNotificationManager{},
		Store:               store,
	}
	if c.txOutChecker != nil {
		cfg.TxOutChecker = c.txOutChecker
	}

	f, err := NewFSM(c.ctx, loopIn, cfg, false)
	require.NoError(c.t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(c.ctx, nil)
	}()

	return resultChan
}

// confRegistration waits for the monitor to register for the htlc
// confirmation.
func (c *monitorTestContext) confRegistration() *test.ConfRegistration {
	select {
	case confRegistration := <-c.mockLnd.RegisterConfChannel:
		return confRegistration

	case <-c.ctx.Done():
		c.t.Fatalf("htlc conf registration not received: %v",
			c.ctx.Err())

		return nil
	}
}

// result waits for the monitor to return.
func (c *monitorTestContext) result(
	resultChan chan fsm.EventType) fsm.EventType {

	select {
	case event := <-resultChan:
		return event

	case <-c.ctx.Done():
		c.t.Fatalf("monitor action did not return: %v", c.ctx.Err())

		return ""
	}
}

// stop stops the monitor and waits for it to exit.
func (c *monitorTestContext) stop(resultChan chan fsm.EventType) {
	c.cancel()

	select {
	case <-resultChan:
	case <-time.After(3 * time.Second):
		c.t.Fatal("monitor action did not exit")
	}
}

// loadedDeposits returns the ids of the deposits the swap loads from the
// store.
func (c *monitorTestContext) loadedDeposits(
	loopIn *StaticAddressLoopIn) []deposit.ID {

	loaded, err := c.swapStore.GetLoopInByHash(
		c.t.Context(), loopIn.SwapHash,
	)
	require.NoError(c.t, err)

	ids := make([]deposit.ID, 0, len(loaded.Deposits))
	for _, d := range loaded.Deposits {
		ids = append(ids, d.ID)
	}

	return ids
}

// settingDepositManager returns a deposit manager that owns the deposits,
// moves them to the requested state, and rejects moving a deposit in one of
// the rejected states.
func settingDepositManager(transitionChan chan depositTransition,
	deposits []*deposit.Deposit,
	rejected ...fsm.StateType) *recordingDepositManager {

	depositMgr := newRecordingDepositManager(deposits...)
	depositMgr.transitionChan = transitionChan
	depositMgr.transition = func(deposits []*deposit.Deposit,
		_ fsm.EventType, state fsm.StateType) error {

		for _, d := range deposits {
			if slices.ContainsFunc(rejected, d.IsInState) {
				return errors.New("final state")
			}
			d.SetState(state)
		}

		return nil
	}

	return depositMgr
}

// depositSpendTx returns a tx without outputs that spends exactly the deposits
// that the swap signed its htlc tx for.
func depositSpendTx(t *testing.T, loopIn *StaticAddressLoopIn) *wire.MsgTx {
	t.Helper()

	outpoints, err := loopIn.signedOutpoints()
	require.NoError(t, err)

	tx := wire.NewMsgTx(2)
	for _, outpoint := range outpoints {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
	}

	return tx
}

// htlcConfFor returns the confirmation of an htlc tx that spends exactly the
// deposits that the swap signed it for and pays to its htlc.
func htlcConfFor(t *testing.T,
	loopIn *StaticAddressLoopIn) *chainntnfs.TxConfirmation {

	t.Helper()

	htlc, err := loopIn.getHtlc(&chaincfg.RegressionNetParams)
	require.NoError(t, err)

	tx := depositSpendTx(t, loopIn)
	tx.AddTxOut(wire.NewTxOut(1_000_000, htlc.PkScript))

	return &chainntnfs.TxConfirmation{Tx: tx}
}

// TestMonitorInvoiceAndHtlcTxRemapsReclaimedDeposits tests that a swap maps
// the deposits another swap selected back to itself when its htlc tx confirms
// after its payment deadline, but not when another tx pays to its htlc script.
func TestMonitorInvoiceAndHtlcTxRemapsReclaimedDeposits(t *testing.T) {
	for _, ownHtlc := range []bool{true, false} {
		name := "own htlc tx"
		if !ownHtlc {
			name = "other tx paying to the htlc script"
		}

		t.Run(name, func(t *testing.T) {
			testRemapsReclaimedDeposits(t, ownHtlc)
		})
	}
}

// decisionStore is a loop-in store that records risk decisions like the real
// store, so that a monitor with a store keeps its risk decisions. It also
// accepts deposit mappings.
type decisionStore struct {
	mockStore
}

// newDecisionStore returns a decision store that holds the swap.
func newDecisionStore(swapHash lntypes.Hash) *decisionStore {
	s := &decisionStore{}
	s.loopIns = map[lntypes.Hash]*StaticAddressLoopIn{
		swapHash: {SwapHash: swapHash},
	}

	return s
}

// RecordStaticAddressRiskDecision records the decision and its time.
func (s *decisionStore) RecordStaticAddressRiskDecision(_ context.Context,
	swapHash lntypes.Hash, decision ConfirmationRiskDecision) error {

	loopIn, ok := s.loopIns[swapHash]
	if !ok {
		return errors.New("swap not found")
	}
	loopIn.ConfirmationRiskDecision = decision
	loopIn.ConfirmationRiskDecisionTime = time.Now()

	return nil
}

// mapRecordingStore is a loop-in store that counts the calls that map
// deposits to swaps.
type mapRecordingStore struct {
	*SqlStore

	mapCalls atomic.Int32
}

// BatchMapDepositsToSwapHashes counts the call and maps the deposits.
func (s *mapRecordingStore) BatchMapDepositsToSwapHashes(ctx context.Context,
	depositsToHashes map[deposit.ID]lntypes.Hash) error {

	s.mapCalls.Add(1)

	return s.SqlStore.BatchMapDepositsToSwapHashes(ctx, depositsToHashes)
}

// testRemapsReclaimedDeposits runs the takeover. If ownHtlc isn't set, another
// tx than swap A's htlc tx confirms at its htlc script.
func testRemapsReclaimedDeposits(t *testing.T, ownHtlc bool) {
	c := newMonitorTestContext(t)

	// Swap A selects d1 and d2. Swap B later selects d2 and d3.
	d1, d2, d3 := c.newDeposit(7), c.newDeposit(8), c.newDeposit(9)

	swapA := c.newLoopIn(0xa, d1, d2)
	swapA.PaymentTimeoutSeconds = 1
	require.NoError(t, c.swapStore.CreateLoopIn(c.ctx, swapA))

	// The server accepted the risk an hour ago, so the payment deadline has
	// passed. It's set directly, since a sent decision gets the current time.
	swapA.ConfirmationRiskDecision = ConfirmationRiskDecisionAccepted
	swapA.ConfirmationRiskDecisionTime = time.Now().Add(-time.Hour)

	htlcConf := htlcConfFor(t, swapA)
	if !ownHtlc {
		htlcConf.Tx.TxIn[0].PreviousOutPoint = wire.OutPoint{
			Hash: chainhash.Hash{0xff},
		}
	}

	// The wallet lists the original deposits as unspent, which the monitor
	// requires before it gives them back.
	c.txOutChecker = unspentDepositChecker([]*deposit.Deposit{d1, d2})

	depositMgr := settingDepositManager(
		make(chan depositTransition, 2),
		[]*deposit.Deposit{d1, d2},
	)
	store := &mapRecordingStore{SqlStore: c.swapStore}
	resultChan := c.startMonitor(
		swapA, invoices.ContractOpen, depositMgr, store,
	)

	select {
	case <-c.mockLnd.SingleInvoiceSubcribeChannel:
	case <-c.ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", c.ctx.Err())
	}

	confRegistration := c.confRegistration()

	// The payment deadline passed before the htlc confirmed, so swap A
	// gives the deposits back.
	select {
	case <-c.mockLnd.FailInvoiceChannel:
	case <-c.ctx.Done():
		t.Fatalf("invoice was not canceled: %v", c.ctx.Err())
	}

	select {
	case transition := <-depositMgr.transitionChan:
		require.Equal(t, deposit.Deposited, transition.state)

	case <-c.ctx.Done():
		t.Fatalf("deposits were not given back: %v", c.ctx.Err())
	}

	// Swap B selects d2 and d3, which maps them to swap B.
	d2.SetState(deposit.LoopingIn)
	swapB := c.newLoopIn(0xb, d2, d3)
	require.NoError(t, c.swapStore.CreateLoopIn(c.ctx, swapB))

	// Something confirms at swap A's htlc script after all.
	confRegistration.ConfChan <- htlcConf

	// A tx that pays to the htlc script but doesn't spend swap A's
	// deposits isn't its htlc tx. Swap A ignores it, watches the spend of
	// its deposits instead and doesn't take them back.
	if !ownHtlc {
		select {
		case <-c.mockLnd.RegisterSpendChannel:
		case <-c.ctx.Done():
			t.Fatalf("deposit spend not watched: %v", c.ctx.Err())
		}

		require.Never(t, func() bool {
			return len(depositMgr.transitionChan) > 0 ||
				store.mapCalls.Load() > 0
		}, 500*time.Millisecond, 10*time.Millisecond)

		c.stop(resultChan)
		require.ElementsMatch(
			t, []deposit.ID{d2.ID, d3.ID},
			c.loadedDeposits(swapB),
		)
		require.Equal(
			t, []deposit.ID{d1.ID}, c.loadedDeposits(swapA),
		)

		return
	}

	// Swap A's htlc tx confirmed, so swap A takes its deposits back to
	// sweep the htlc timeout path.
	select {
	case transition := <-depositMgr.transitionChan:
		require.Equal(t, deposit.SweepHtlcTimeout, transition.state)

	case <-c.ctx.Done():
		t.Fatalf("deposits were not taken back: %v", c.ctx.Err())
	}

	// Swap A loads d1 and d2 from the store, and swap B only d3.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.ElementsMatch(
			ct, []deposit.ID{d1.ID, d2.ID}, c.loadedDeposits(swapA),
		)
	}, testTimeout, 10*time.Millisecond)
	require.Equal(t, []deposit.ID{d3.ID}, c.loadedDeposits(swapB))
	c.stop(resultChan)
}

// TestMonitorInvoiceAndHtlcTxMapsOnlyLockedDeposits tests that a deposit
// another swap completed with stays mapped to that swap, and that an already
// locked deposit is mapped again.
func TestMonitorInvoiceAndHtlcTxMapsOnlyLockedDeposits(t *testing.T) {
	tests := []struct {
		name  string
		state fsm.StateType
		owner lntypes.Hash
	}{
		{
			name:  "completed by the other swap",
			state: deposit.LoopedIn,
			owner: lntypes.Hash{0xb},
		},
		{
			name:  "already locked for the htlc timeout sweep",
			state: deposit.SweepHtlcTimeout,
			owner: lntypes.Hash{0xa},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMonitorTestContext(t)
			d := c.newDeposit(8)

			// Swap A gave the deposit back, and swap B selected it.
			swapA := c.newLoopIn(0xa, d)
			require.NoError(t, c.swapStore.CreateLoopIn(c.ctx, swapA))
			swapB := c.newLoopIn(0xb, d)
			require.NoError(t, c.swapStore.CreateLoopIn(c.ctx, swapB))
			d.SetState(tc.state)

			htlcConf := htlcConfFor(t, swapA)

			// Swap A's invoice was canceled. The deposit manager
			// rejects moving a completed deposit.
			resultChan := c.startMonitor(
				swapA, invoices.ContractCanceled,
				settingDepositManager(
					make(chan depositTransition, 4),
					[]*deposit.Deposit{d}, deposit.LoopedIn,
				),
				c.swapStore,
			)

			// Swap A's htlc tx confirms.
			c.confRegistration().ConfChan <- htlcConf

			owners := func() []lntypes.Hash {
				var owners []lntypes.Hash
				for _, swap := range []*StaticAddressLoopIn{
					swapA, swapB,
				} {
					if len(c.loadedDeposits(swap)) > 0 {
						owners = append(owners, swap.SwapHash)
					}
				}

				return owners
			}

			// A completed deposit can't be moved, so the monitor
			// retries once the attempt failed. A locked deposit
			// isn't moved again, and the monitor keeps running.
			if tc.state == deposit.LoopedIn {
				require.Equal(t, OnRecover, c.result(resultChan))
				require.Equal(t, []lntypes.Hash{tc.owner}, owners())

				return
			}

			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				assert.Equal(
					ct, []lntypes.Hash{tc.owner}, owners(),
				)
			}, testTimeout, 10*time.Millisecond)

			c.stop(resultChan)
		})
	}
}

// TestMonitorInvoiceAndHtlcTxAddsDepositsAfterRestart tests that a swap that
// loads only some of its signed deposits after a restart, because loopd stopped
// after locking them but before mapping them, locks and maps all of them in
// signed order once its own htlc tx confirms.
func TestMonitorInvoiceAndHtlcTxAddsDepositsAfterRestart(t *testing.T) {
	tests := []struct {
		name string

		// missingFirst is set if the missing deposit d2 comes first in
		// signed order.
		missingFirst bool
	}{
		{
			name: "missing deposit signed last",
		},
		{
			name:         "missing deposit signed first",
			missingFirst: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMonitorTestContext(t)

			// Swap A selected d1 and d2. After its payment deadline
			// swap B selected d2 and d3, which mapped d2 to swap B.
			// Then swap A's htlc tx confirmed, and loopd stopped
			// after locking d1 and d2 for A's htlc timeout sweep.
			d1, d2, d3 := c.newDeposit(7), c.newDeposit(8),
				c.newDeposit(9)

			signed := []*deposit.Deposit{d1, d2}
			if tc.missingFirst {
				signed = []*deposit.Deposit{d2, d1}
			}
			swapA := c.newLoopIn(0xa, signed...)
			require.NoError(t, c.swapStore.CreateLoopIn(c.ctx, swapA))
			swapB := c.newLoopIn(0xb, d2, d3)
			require.NoError(t, c.swapStore.CreateLoopIn(c.ctx, swapB))
			d1.SetState(deposit.SweepHtlcTimeout)
			d2.SetState(deposit.SweepHtlcTimeout)

			// After the restart swap A only loads d1, but its htlc
			// tx spends d1 and d2.
			loaded, err := c.swapStore.GetLoopInByHash(
				c.ctx, swapA.SwapHash,
			)
			require.NoError(t, err)
			require.Len(t, loaded.Deposits, 1)
			require.Equal(t, d1.ID, loaded.Deposits[0].ID)
			swapA.Deposits = []*deposit.Deposit{d1}
			htlcConf := htlcConfFor(t, swapA)
			require.Len(t, htlcConf.Tx.TxIn, 2)

			depositMgr := settingDepositManager(
				make(chan depositTransition, 4),
				[]*deposit.Deposit{d1, d2, d3},
			)
			resultChan := c.startMonitor(
				swapA, invoices.ContractCanceled, depositMgr,
				c.swapStore,
			)

			c.confRegistration().ConfChan <- htlcConf

			// Swap A maps both deposits, and swap B keeps only d3.
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				assert.ElementsMatch(
					ct, []deposit.ID{d1.ID, d2.ID},
					c.loadedDeposits(swapA),
				)
			}, testTimeout, 10*time.Millisecond)
			require.Equal(t, []deposit.ID{d3.ID}, c.loadedDeposits(swapB))

			c.stop(resultChan)

			// The deposits are all signed deposits, in signed order.
			require.Equal(t, signed, swapA.Deposits)
			require.Equal(
				t, []wire.OutPoint{
					htlcConf.Tx.TxIn[0].PreviousOutPoint,
					htlcConf.Tx.TxIn[1].PreviousOutPoint,
				}, swapA.Outpoints(),
			)
		})
	}
}

// TestMonitorInvoiceAndHtlcTxFindsHtlcTxAfterExpiry tests a swap that restarts
// after its htlc expired, with its deposits locked for the htlc timeout sweep
// but only some of them still mapped to it. The monitor then leaves for the
// sweep on the first block, before any htlc confirmation, so it looks up the
// confirmed htlc tx in the wallet to add and map the missing deposits.
func TestMonitorInvoiceAndHtlcTxFindsHtlcTxAfterExpiry(t *testing.T) {
	tests := []struct {
		name string

		// paysToHtlc is set if the wallet tx that spends the deposits
		// pays to the htlc of the swap.
		paysToHtlc bool
	}{
		{
			name:       "wallet has the htlc tx",
			paysToHtlc: true,
		},
		{
			name: "wallet tx doesn't pay to the htlc",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newMonitorTestContext(t)

			// Swap A selected d1 and d2. Swap B then selected d2
			// and d3, which mapped d2 to swap B. Swap A's htlc tx
			// confirmed, and loopd stopped after locking d1 and d2
			// for A's htlc timeout sweep.
			d1, d2, d3 := c.newDeposit(7), c.newDeposit(8),
				c.newDeposit(9)

			signed := []*deposit.Deposit{d1, d2}
			swapA := c.newLoopIn(0xa, signed...)
			swapA.HtlcCltvExpiry = c.mockLnd.Height
			require.NoError(t, c.swapStore.CreateLoopIn(c.ctx, swapA))
			swapB := c.newLoopIn(0xb, d2, d3)
			require.NoError(t, c.swapStore.CreateLoopIn(c.ctx, swapB))
			d1.SetState(deposit.SweepHtlcTimeout)
			d2.SetState(deposit.SweepHtlcTimeout)

			// After the restart swap A only loads d1.
			swapA.Deposits = []*deposit.Deposit{d1}

			// The wallet lists a confirmed tx that spends d1 and
			// d2.
			htlc, err := swapA.getHtlc(c.mockLnd.ChainParams)
			require.NoError(t, err)

			walletTx := depositSpendTx(t, swapA)
			pkScript := []byte{0x51}
			if tc.paysToHtlc {
				pkScript = htlc.PkScript
			}
			walletTx.AddTxOut(wire.NewTxOut(1_000_000, pkScript))

			checker := &recordingTxOutChecker{
				spendingTxs: map[wire.OutPoint]*wire.MsgTx{
					d1.OutPoint: walletTx,
				},
			}
			c.txOutChecker = checker

			depositMgr := settingDepositManager(
				make(chan depositTransition, 4),
				[]*deposit.Deposit{d1, d2, d3},
			)

			// Swap A is known to own the timeout lock of d1.
			// Without a known owner the monitor waits for the
			// htlc confirmation instead.
			depositMgr.claims = map[wire.OutPoint]lntypes.Hash{
				d1.OutPoint: swapA.SwapHash,
			}
			resultChan := c.startMonitor(
				swapA, invoices.ContractCanceled, depositMgr,
				c.swapStore,
			)

			// The htlc expired, and no htlc confirmation arrives.
			c.confRegistration()
			require.NoError(
				t, c.mockLnd.NotifyHeight(c.mockLnd.Height+1),
			)
			require.Equal(t, OnSweepHtlcTimeout, c.result(resultChan))
			require.Contains(
				t, checker.spendingTxOutpoints, d1.OutPoint,
			)

			if !tc.paysToHtlc {
				// Nothing is known to spend the deposits, so
				// swap B keeps d2.
				require.Equal(
					t, []*deposit.Deposit{d1}, swapA.Deposits,
				)
				require.ElementsMatch(
					t, []deposit.ID{d2.ID, d3.ID},
					c.loadedDeposits(swapB),
				)

				return
			}

			// Swap A sweeps with both deposits, and both are
			// mapped to it.
			require.Equal(t, signed, swapA.Deposits)
			require.ElementsMatch(
				t, []deposit.ID{d1.ID, d2.ID},
				c.loadedDeposits(swapA),
			)
			require.Equal(
				t, []deposit.ID{d3.ID}, c.loadedDeposits(swapB),
			)
		})
	}
}

// TestMonitorInvoiceAndHtlcTxNilDeposit tests that a nil deposit makes the
// monitor retry before it moves any deposit.
func TestMonitorInvoiceAndHtlcTxNilDeposit(t *testing.T) {
	c := newMonitorTestContext(t)

	depositMgr := &recordingDepositManager{}
	loopIn := c.newLoopIn(0xc, c.newDeposit(9), nil)
	resultChan := c.startMonitor(
		loopIn, invoices.ContractCanceled, depositMgr, &mockStore{},
	)

	c.confRegistration().ConfChan <- htlcConfFor(t, loopIn)
	require.Equal(t, OnRecover, c.result(resultChan))

	// No deposit was moved, not even the one that isn't nil.
	require.Empty(t, depositMgr.transitions)
}

// failingMapStore is a store that fails to map deposits to swaps.
type failingMapStore struct {
	mockStore
}

// BatchMapDepositsToSwapHashes returns an error.
func (s *failingMapStore) BatchMapDepositsToSwapHashes(context.Context,
	map[deposit.ID]lntypes.Hash) error {

	return errors.New("map failed")
}

// TestMonitorInvoiceAndHtlcTxRetriesFailedMapping tests that the monitor
// retries if it can't map the deposits to the swap.
func TestMonitorInvoiceAndHtlcTxRetriesFailedMapping(t *testing.T) {
	for _, state := range []fsm.StateType{
		deposit.LoopingIn, deposit.SweepHtlcTimeout,
	} {
		t.Run(string(state), func(t *testing.T) {
			c := newMonitorTestContext(t)

			d := c.newDeposit(10)
			d.SetState(state)
			loopIn := c.newLoopIn(0xd, d)
			htlcConf := htlcConfFor(t, loopIn)

			resultChan := c.startMonitor(
				loopIn, invoices.ContractCanceled,
				settingDepositManager(
					nil, []*deposit.Deposit{d},
				),
				&failingMapStore{},
			)

			c.confRegistration().ConfChan <- htlcConf
			require.Equal(t, OnRecover, c.result(resultChan))

			// The deposit stays locked for the htlc timeout sweep.
			require.True(t, d.IsInState(deposit.SweepHtlcTimeout))
		})
	}
}

// TestMonitorInvoiceAndHtlcTxRemapsAfterLegacyFallback tests the takeover
// without a recorded risk decision, so the legacy confirmation fallback
// reloads the swap's deposits from the store before its htlc tx confirms.
func TestMonitorInvoiceAndHtlcTxRemapsAfterLegacyFallback(t *testing.T) {
	c := newMonitorTestContext(t)

	// Swap A selects d1 and d2. Swap B later selects d2 and d3. All of
	// them have enough confirmations for the legacy payment deadline.
	d1, d2, d3 := c.newDeposit(7), c.newDeposit(8), c.newDeposit(9)
	for _, d := range []*deposit.Deposit{d1, d2, d3} {
		d.ConfirmationHeight = int64(c.mockLnd.Height) - 10
	}

	swapA := c.newLoopIn(0xa, d1, d2)
	swapA.PaymentTimeoutSeconds = 1
	require.NoError(t, c.swapStore.CreateLoopIn(c.ctx, swapA))

	// The wallet lists the original deposits as unspent, which the monitor
	// requires before it gives them back.
	c.txOutChecker = unspentDepositChecker([]*deposit.Deposit{d1, d2})

	// The deposit manager hands out copies from DepositsForOutpoints, like
	// a store read, and only moves its own deposits.
	depositMgr := settingDepositManager(
		make(chan depositTransition, 2),
		[]*deposit.Deposit{d1, d2, d3},
	)
	resultChan := c.startMonitor(
		swapA, invoices.ContractOpen, depositMgr, c.swapStore,
	)

	select {
	case <-c.mockLnd.SingleInvoiceSubcribeChannel:
	case <-c.ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", c.ctx.Err())
	}

	confRegistration := c.confRegistration()

	// The legacy fallback starts the payment deadline, which passes before
	// the htlc confirms, so swap A gives the deposits back.
	select {
	case <-c.mockLnd.FailInvoiceChannel:
	case <-c.ctx.Done():
		t.Fatalf("invoice was not canceled: %v", c.ctx.Err())
	}

	select {
	case transition := <-depositMgr.transitionChan:
		require.Equal(t, deposit.Deposited, transition.state)

	case <-c.ctx.Done():
		t.Fatalf("deposits were not given back: %v", c.ctx.Err())
	}

	// Swap B selects d2 and d3, then swap A's htlc tx confirms.
	d2.SetState(deposit.LoopingIn)
	swapB := c.newLoopIn(0xb, d2, d3)
	require.NoError(t, c.swapStore.CreateLoopIn(c.ctx, swapB))

	confRegistration.ConfChan <- htlcConfFor(t, swapA)

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.ElementsMatch(
			ct, []deposit.ID{d1.ID, d2.ID}, c.loadedDeposits(swapA),
		)
	}, testTimeout, 10*time.Millisecond)
	require.Equal(t, []deposit.ID{d3.ID}, c.loadedDeposits(swapB))

	c.stop(resultChan)
}

// TestMonitorInvoiceAndHtlcTxClaimsReclaimedDeposits tests that a swap whose
// deposit was locked by another swap becomes its owner in the deposit manager
// and in the store once its own htlc tx confirms. If that htlc tx is then
// reorged out and the wallet lists the deposit as unspent again, the swap
// must not take the other swap for the owner of the htlc timeout sweep and
// time out. It keeps monitoring and sweeps once the htlc tx confirms again.
func TestMonitorInvoiceAndHtlcTxClaimsReclaimedDeposits(t *testing.T) {
	funding := newMonitorRecoveryFunding()
	h := newExpiryRecoveryHarnessWithFunding(
		t, deposit.LoopingIn, true, funding,
	)
	swapA, swapB := h.f.loopIn.SwapHash, lntypes.Hash{9}
	h.lnd.SetInvoice(&lndclient.Invoice{
		Hash: swapA, State: invoices.ContractCanceled,
	})

	// Swap A gave the deposit back and swap B locked it, which mapped it
	// to swap B.
	require.NoError(t, h.manager.ReleaseDepositsForSwap(
		h.ctx, h.f.loopIn.Deposits, swapA,
	))
	require.NoError(t, h.manager.LockDepositsForSwap(
		h.ctx, h.f.loopIn.Deposits, swapB,
	))
	require.NoError(t, h.db.BaseDB.MapDepositToSwap(
		h.ctx, sqlc.MapDepositToSwapParams{
			DepositID: h.dep.ID[:], SwapHash: swapB[:],
		},
	))
	owner, known := h.manager.GetLoopInClaim(h.dep.OutPoint)
	require.True(t, known)
	require.Equal(t, swapB, owner)

	history := newMonitorRecoveryHistory()
	h.f.cfg.TxOutChecker = NewLndTxOutChecker(history)
	notifier := &monitorRecoveryReorgNotifier{
		expiryRecoveryNotifier: h.notifier,
		reorgs:                 make(chan chan struct{}, 4),
	}
	h.f.cfg.ChainNotifier = notifier

	// Swap A's htlc tx confirms after all.
	htlc, err := h.f.loopIn.createHtlcTx(
		h.lnd.ChainParams, h.f.loopIn.HtlcTxFeeRate, 1,
	)
	require.NoError(t, err)
	history.setTransactions([]lndclient.Transaction{
		{Tx: funding, Confirmations: 1_601},
		{Tx: htlc, Confirmations: defaultConfTarget},
	})
	result, conf, blocks := h.monitor(t)
	reorg := expiryRecoveryReceive(t, h.ctx, notifier.reorgs)
	h.sendConfirmation(t, conf, htlc, expiryRecoveryHeight-2)

	// The htlc tx is reorged out. The monitor handles the reorg after it
	// locked and mapped the deposit, so swap A owns it by now.
	reorg <- struct{}{}
	reconfirmation := expiryRecoveryReceive(t, h.ctx, h.notifier.confs)
	h.requireActive(t, deposit.SweepHtlcTimeout)

	owner, known = h.manager.GetLoopInClaim(h.dep.OutPoint)
	require.True(t, known)
	require.Equal(t, swapA, owner)

	stored, err := h.db.BaseDB.SwapHashForDepositID(h.ctx, h.dep.ID[:])
	require.NoError(t, err)
	require.Equal(t, swapA[:], stored)

	// The wallet forgets the htlc tx, so the deposit is unspent again. The
	// htlc has expired, but swap A still owns the htlc timeout sweep and
	// keeps monitoring.
	history.setTransactions([]lndclient.Transaction{{
		Tx: funding, Confirmations: 1_601,
	}})
	h.sendBlock(t, blocks, expiryRecoveryHeight)
	h.sendBlock(t, blocks, expiryRecoveryHeight+1)
	h.requireActive(t, deposit.SweepHtlcTimeout)

	// The htlc tx confirms again and swap A sweeps it.
	h.sendConfirmation(t, reconfirmation, htlc, expiryRecoveryHeight-2)
	h.sendBlock(t, blocks, expiryRecoveryHeight+2)
	require.Equal(
		t, OnSweepHtlcTimeout, expiryRecoveryReceive(t, h.ctx, result),
	)

	owner, known = h.manager.GetLoopInClaim(h.dep.OutPoint)
	require.True(t, known)
	require.Equal(t, swapA, owner)
}
