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
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
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

	return &monitorTestContext{
		t:            t,
		ctx:          ctx,
		cancel:       cancel,
		mockLnd:      test.NewMockLnd(),
		clientPubKey: clientPubKey,
		serverPubKey: serverPubKey,
		addr:         addr,
		depositStore: deposit.NewSqlStore(testDb.BaseDB),
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

// htlcConfFor returns the confirmation of an htlc tx that spends exactly the
// deposits that the swap signed it for.
func htlcConfFor(t *testing.T,
	loopIn *StaticAddressLoopIn) *chainntnfs.TxConfirmation {

	t.Helper()

	outpoints, err := loopIn.signedOutpoints()
	require.NoError(t, err)

	tx := wire.NewMsgTx(2)
	for _, outpoint := range outpoints {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
	}

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

	// Something confirms at swap A's htlc script after all, so swap A
	// takes its deposits back to sweep the htlc timeout path.
	confRegistration.ConfChan <- htlcConf

	select {
	case transition := <-depositMgr.transitionChan:
		require.Equal(t, deposit.SweepHtlcTimeout, transition.state)

	case <-c.ctx.Done():
		t.Fatalf("deposits were not taken back: %v", c.ctx.Err())
	}

	// If swap A's htlc tx confirmed, swap A loads d1 and d2 from the store,
	// and swap B only d3. Otherwise swap A might not have spent d2, so it
	// stays mapped to swap B.
	if ownHtlc {
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			assert.ElementsMatch(
				ct, []deposit.ID{d1.ID, d2.ID},
				c.loadedDeposits(swapA),
			)
		}, testTimeout, 10*time.Millisecond)
		require.Equal(t, []deposit.ID{d3.ID}, c.loadedDeposits(swapB))
		c.stop(resultChan)

		return
	}

	// The monitor would map the deposits right after locking them, so it
	// would have done so by now.
	require.Never(t, func() bool {
		return store.mapCalls.Load() > 0
	}, 500*time.Millisecond, 10*time.Millisecond)

	c.stop(resultChan)
	require.ElementsMatch(
		t, []deposit.ID{d2.ID, d3.ID}, c.loadedDeposits(swapB),
	)
	require.Equal(t, []deposit.ID{d1.ID}, c.loadedDeposits(swapA))
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

// TestMonitorInvoiceAndHtlcTxNilDeposit tests that a nil deposit makes the
// monitor retry before it moves any deposit.
func TestMonitorInvoiceAndHtlcTxNilDeposit(t *testing.T) {
	c := newMonitorTestContext(t)

	depositMgr := &recordingDepositManager{}
	resultChan := c.startMonitor(
		c.newLoopIn(0xc, c.newDeposit(9), nil), invoices.ContractCanceled,
		depositMgr, &mockStore{},
	)

	c.confRegistration().ConfChan <- nil
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
