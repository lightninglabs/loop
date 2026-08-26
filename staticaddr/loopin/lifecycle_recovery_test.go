package loopin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestReconcileLoopInDeposits repairs pre-store crashes and failed final-state
// writes while keeping every pending swap's deposit ownership intact.
func TestReconcileLoopInDeposits(t *testing.T) {
	for _, test := range []struct {
		name  string
		owner fsm.StateType
		want  fsm.StateType
	}{
		{"orphan", fsm.EmptyState, deposit.Deposited},
		{"failed", Failed, deposit.Deposited},
		{"succeeded", Succeeded, deposit.LoopedIn},
		{"final write failed", SucceededTransitioningFailed, deposit.LoopedIn},
		{"payment received", PaymentReceived, deposit.LoopedIn},
		{"pending", MonitorInvoiceAndHtlcTx, deposit.LoopingIn},
		{"refund pending", SweepHtlcTimeout, deposit.LoopingIn},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := &deposit.Deposit{ID: deposit.ID{1},
				Hash: chainhash.Hash{1}}
			d.SetState(deposit.LoopingIn)
			deposits := &recoveryDepositManager{
				recordingDepositManager: newRecordingDepositManager(d),
			}
			store := &mockStore{loopIns: make(map[lntypes.Hash]*StaticAddressLoopIn)}
			m := &Manager{cfg: &Config{DepositManager: deposits, Store: store}}
			var swaps []*StaticAddressLoopIn
			if test.owner != fsm.EmptyState {
				hash := lntypes.Hash{1}
				d.SwapHash = &hash
				swap := &StaticAddressLoopIn{SwapHash: hash,
					Deposits: []*deposit.Deposit{d}}
				swap.SetState(test.owner)
				store.loopIns[hash] = swap
				if test.owner == MonitorInvoiceAndHtlcTx ||
					test.owner == SweepHtlcTimeout {

					swaps = append(swaps, swap)
				}
			}
			require.NoError(t, m.reconcileLoopInDeposits(t.Context(), swaps))
			require.Equal(t, test.want, d.GetState())
		})
	}
}

// TestRecoverLoopInStoreErrorDoesNotRelease ensures an unavailable owner store
// aborts startup rather than treating every lock as an orphan.
func TestRecoverLoopInStoreErrorDoesNotRelease(t *testing.T) {
	want := errors.New("owner database unavailable")
	d := &deposit.Deposit{ID: deposit.ID{1}}
	d.SetState(deposit.LoopingIn)
	deposits := &recoveryDepositManager{
		recordingDepositManager: newRecordingDepositManager(d),
	}
	m := &Manager{cfg: &Config{
		DepositManager: deposits,
		Store:          &unavailableRecoveryStore{err: want},
	}}
	require.ErrorIs(t, m.recoverLoopIns(t.Context()), want)
	require.Empty(t, deposits.transitions)
	require.Equal(t, deposit.LoopingIn, d.GetState())
}

// TestReconcileLoopInOwnerLookupError preserves a mapped lock when its final
// owner cannot be retrieved, rather than mistaking a database failure for absence.
func TestReconcileLoopInOwnerLookupError(t *testing.T) {
	want := errors.New("owner lookup failed")
	hash := lntypes.Hash{1}
	d := &deposit.Deposit{ID: deposit.ID{1}, SwapHash: &hash}
	d.SetState(deposit.LoopingIn)
	deposits := &recoveryDepositManager{
		recordingDepositManager: newRecordingDepositManager(d),
	}
	m := &Manager{cfg: &Config{DepositManager: deposits,
		Store: &unavailableRecoveryStore{err: want}}}
	require.ErrorIs(t, m.reconcileLoopInDeposits(t.Context(), nil), want)
	require.Empty(t, deposits.transitions)
	require.Equal(t, deposit.LoopingIn, d.GetState())
}

// TestReplacedFundingReleasesOtherDeposits keeps a positively unspent sibling
// usable when another selected deposit's funding disappeared after signing.
func TestReplacedFundingReleasesOtherDeposits(t *testing.T) {
	missing := &deposit.Deposit{
		Hash: chainhash.Hash{2}, Value: 100_000,
	}
	h := newExpiryRecoveryHarness(t, deposit.LoopingIn, true, missing)
	h.lnd.SetInvoice(&lndclient.Invoice{
		Hash: h.f.loopIn.SwapHash, State: invoices.ContractCanceled,
	})
	h.f.cfg.TxOutChecker = &expiryRecoveryChecker{
		outputs: map[wire.OutPoint]*wire.TxOut{
			h.dep.OutPoint: {Value: int64(h.dep.Value),
				PkScript: h.dep.AddressParams.PkScript},
		},
	}
	result, _, blocks := h.monitor(t)
	h.sendBlock(t, blocks, expiryRecoveryHeight)
	require.Equal(t, OnSwapTimedOut,
		expiryRecoveryReceive(t, h.ctx, result))
	h.requireStoredState(t, deposit.Deposited)
	// The vanished record is released too, allowing normal reconciliation
	// to deactivate it instead of leaving an ownerless LoopingIn record.
	stored, err := h.store.GetDeposit(h.ctx, missing.ID)
	require.NoError(t, err)
	require.Equal(t, deposit.Deposited, stored.GetState())
	require.NoError(t, h.manager.EnsureDepositsFresh(h.ctx))
	_, active := h.manager.AllStringOutpointsActiveDeposits(
		[]string{missing.OutPoint.String()}, fsm.EmptyState,
	)
	require.False(t, active)
}

// TestReusedDepositsDoNotKeepOldSwapPending resolves an old canceled swap on
// confirmed conflict proof without disturbing a retry's pre-store ownership.
func TestReusedDepositsDoNotKeepOldSwapPending(t *testing.T) {
	h := newExpiryRecoveryHarness(t, deposit.LoopingIn, false)
	a := h.f.loopIn.SwapHash
	b := lntypes.Hash{9}
	require.NoError(t, h.manager.ReleaseLoopInDeposits(
		h.ctx, h.f.loopIn.Deposits, a,
	))
	require.NoError(t, h.manager.LockDepositsForLoopIn(
		h.ctx, h.f.loopIn.Deposits, b,
	))
	spend := wire.NewMsgTx(2)
	spend.AddTxIn(&wire.TxIn{PreviousOutPoint: h.dep.OutPoint})
	spend.AddTxOut(wire.NewTxOut(199_000, []byte{0x51}))
	h.f.cfg.TxOutChecker = NewLndTxOutChecker(&mockTxListLightningClient{
		txs: []lndclient.Transaction{{Tx: spend, Confirmations: defaultConfTarget}},
	})
	h.lnd.SetInvoice(&lndclient.Invoice{
		Hash: a, State: invoices.ContractCanceled,
	})
	result, _, blocks := h.monitor(t)
	h.sendBlock(t, blocks, expiryRecoveryHeight)
	require.Equal(t, OnSwapTimedOut,
		expiryRecoveryReceive(t, h.ctx, result))
	h.requireActive(t, deposit.LoopingIn)
	require.NoError(t, h.manager.ReleaseLoopInDeposits(
		h.ctx, h.f.loopIn.Deposits, b,
	))
	h.requireActive(t, deposit.Deposited)
}

// TestCanceledLoopInRetriesDepositRelease retries a transient availability
// failure before HTLC expiry, including recovery with a canceled invoice.
func TestCanceledLoopInRetriesDepositRelease(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "restart"}[restart],
			func(t *testing.T) {
				h := newExpiryRecoveryHarness(t, deposit.LoopingIn, true)
				h.f.loopIn.PaymentTimeoutSeconds = 1
				h.f.loopIn.ConfirmationRiskDecisionTime = time.Now().Add(-time.Hour)
				if restart {
					h.lnd.SetInvoice(&lndclient.Invoice{Hash: h.f.loopIn.SwapHash,
						State: invoices.ContractCanceled})
				}
				checker := &retryReleaseChecker{
					outputs: map[wire.OutPoint]*wire.TxOut{
						h.dep.OutPoint: {Value: int64(h.dep.Value)},
					},
					calls: make(chan struct{}, 4),
				}
				h.f.cfg.TxOutChecker = checker
				_, _, blocks := h.monitor(t)
				if restart {
					h.sendBlock(t, blocks, 1_500)
				} else {
					expiryRecoveryReceive(t, h.ctx, h.lnd.FailInvoiceChannel)
				}
				expiryRecoveryReceive(t, h.ctx, checker.calls)
				h.sendBlock(t, blocks, 1_501)
				h.sendBlock(t, blocks, 1_502)
				h.requireStoredState(t, deposit.Deposited)
			})
	}
}

type recoveryDepositManager struct{ *recordingDepositManager }

func (m *recoveryDepositManager) GetActiveDepositsInState(state fsm.StateType) (
	[]*deposit.Deposit, error) {

	var deposits []*deposit.Deposit
	for _, d := range m.active {
		if d.IsInState(state) {
			deposits = append(deposits, d)
		}
	}
	return deposits, nil
}

type unavailableRecoveryStore struct {
	StaticAddressLoopInStore

	err error
}

func (s *unavailableRecoveryStore) GetStaticAddressLoopInSwapsByStates(
	context.Context, []fsm.StateType) ([]*StaticAddressLoopIn, error) {

	return nil, s.err
}

func (s *unavailableRecoveryStore) GetLoopInByHash(context.Context,
	lntypes.Hash) (*StaticAddressLoopIn, error) {

	return nil, s.err
}

type retryReleaseChecker struct {
	outputs map[wire.OutPoint]*wire.TxOut
	calls   chan struct{}
	failed  bool
}

func (c *retryReleaseChecker) GetTxOuts(context.Context, []wire.OutPoint) (
	map[wire.OutPoint]*wire.TxOut, error) {

	c.calls <- struct{}{}
	if !c.failed {
		c.failed = true
		return nil, errors.New("temporary wallet failure")
	}
	return c.outputs, nil
}
