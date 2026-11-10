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
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// riskRejectionTest runs the invoice and htlc monitor of a swap whose htlc
// expires at the next block.
type riskRejectionTest struct {
	t          *testing.T
	ctx        context.Context
	mockLnd    *test.LndMockServices
	swapHash   lntypes.Hash
	f          *FSM
	depositMgr *recordingDepositManager
	riskMgr    *mockNotificationManager
	txOuts     *recordingTxOutChecker
}

// invoicesFor returns the invoices client the monitor uses.
type invoicesFor func(*test.LndMockServices) lndclient.InvoicesClient

func newRiskRejectionTest(t *testing.T, ctx context.Context,
	invoicesClient invoicesFor) *riskRejectionTest {

	mockLnd := test.NewMockLnd()
	swapHash := lntypes.Hash{8, 1, 3}
	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapHash,
		State: invoices.ContractOpen,
	})

	f, depositMgr := newInvoiceMonitorTestFSM(
		t, ctx, mockLnd, swapHash, ConfirmationRiskDecisionNone,
		invoicesClient(mockLnd),
	)
	f.loopIn.HtlcCltvExpiry = mockLnd.Height
	f.loopIn.InitiationHeight = riskRejectionInitiationHeight
	f.loopIn.Deposits[0].SetState(deposit.LoopingIn)
	depositMgr.transitionChan = make(chan depositTransition, 4)

	// Use a non-zero outpoint, so the spender lookup checks can tell it
	// apart from a zeroed one. The deposit is unspent until a test spends
	// it.
	f.loopIn.Deposits[0].OutPoint = wire.OutPoint{
		Hash:  chainhash.Hash{7},
		Index: 2,
	}
	outpoint := f.loopIn.Deposits[0].OutPoint
	depositMgr.active = map[wire.OutPoint]*deposit.Deposit{
		outpoint: f.loopIn.Deposits[0],
	}
	f.loopIn.DepositOutpoints = []string{outpoint.String()}
	txOutChecker := &recordingTxOutChecker{
		txOuts: map[wire.OutPoint]*wire.TxOut{outpoint: {}},
	}
	f.cfg.TxOutChecker = txOutChecker

	riskMgr := &mockNotificationManager{
		riskRejected: make(
			chan *swapserverrpc.
				ServerStaticLoopInRiskRejectedNotification, 1,
		),
	}
	f.cfg.NotificationManager = riskMgr

	return &riskRejectionTest{
		t:          t,
		ctx:        ctx,
		mockLnd:    mockLnd,
		swapHash:   swapHash,
		f:          f,
		depositMgr: depositMgr,
		riskMgr:    riskMgr,
		txOuts:     txOutChecker,
	}
}

// spendDeposits makes lnd's wallet list the htlc tx spending the deposits, so
// the deposits are no longer available either.
func (r *riskRejectionTest) spendDeposits() {
	r.txOuts.txOuts = nil
	r.txOuts.htlcSpent = true
}

// replaceDeposits makes the deposits unavailable in lnd's wallet without an
// htlc tx, as after their funding transaction was replaced or another
// transaction spent them. Before the fix, an unavailable deposit was taken as
// spent by the htlc tx.
func (r *riskRejectionTest) replaceDeposits() {
	r.txOuts.txOuts = nil
	r.txOuts.htlcSpent = false
}

// riskRejectionInitiationHeight is the swap's initiation height in the risk
// rejection tests. It is above htlcTxReorgLookback, so the spender lookup's
// start height is bounded.
const riskRejectionInitiationHeight = 1000

// requireSpenderLookup checks that every spender lookup asked about the swap's
// deposits and htlc pkScript, starting htlcTxReorgLookback blocks below the
// swap's initiation height.
func (r *riskRejectionTest) requireSpenderLookup() {
	htlc, err := r.f.loopIn.getHtlc(r.f.cfg.ChainParams)
	require.NoError(r.t, err)

	require.NotEmpty(r.t, r.txOuts.spendOutpoints)
	for i, outpoints := range r.txOuts.spendOutpoints {
		require.Equal(
			r.t, []wire.OutPoint{r.f.loopIn.Deposits[0].OutPoint},
			outpoints,
		)
		require.Equal(r.t, htlc.PkScript, r.txOuts.spendPkScripts[i])
		require.Equal(
			r.t, int32(riskRejectionInitiationHeight-
				htlcTxReorgLookback),
			r.txOuts.spendStartHeights[i],
		)
	}
}

// requireFailed checks that the monitor failed the swap without locking the
// deposits. The FSM then unlocks them in UnlockDeposits.
func (r *riskRejectionTest) requireFailed(resultChan chan fsm.EventType) {
	require.Equal(r.t, fsm.OnError, r.result(resultChan))
	require.ErrorContains(
		r.t, r.f.LastActionError,
		"server rejected confirmation risk wait",
	)
	require.Empty(r.t, r.depositMgr.transitions)
	require.Equal(
		r.t, ConfirmationRiskDecisionRejected,
		r.f.loopIn.ConfirmationRiskDecision,
	)
}

// recoverRejection sets up the swap as loaded from the store after the
// notification manager stored a rejection.
func (r *riskRejectionTest) recoverRejection() {
	r.f.loopIn.ConfirmationRiskDecision = ConfirmationRiskDecisionRejected
}

// result waits for the monitor to exit.
func (r *riskRejectionTest) result(resultChan chan fsm.EventType) fsm.EventType {
	select {
	case event := <-resultChan:
		return event

	case <-r.ctx.Done():
		r.t.Fatalf("monitor did not exit: %v", r.ctx.Err())
	}

	return fsm.NoOp
}

// start runs the monitor and returns its result channel and the htlc
// confirmation registration.
func (r *riskRejectionTest) start(ctx context.Context) (chan fsm.EventType,
	*test.ConfRegistration) {

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- r.f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	select {
	case <-r.mockLnd.SingleInvoiceSubcribeChannel:
	case <-r.ctx.Done():
		r.t.Fatalf("invoice subscription not registered: %v",
			r.ctx.Err())
	}

	select {
	case reg := <-r.mockLnd.RegisterConfChannel:
		return resultChan, reg

	case <-r.ctx.Done():
		r.t.Fatalf("htlc conf registration not received: %v",
			r.ctx.Err())
	}

	return nil, nil
}

// confirmHtlc delivers the htlc confirmation. The second send only fits the
// buffer once the monitor received the first one.
func (r *riskRejectionTest) confirmHtlc(reg *test.ConfRegistration) {
	reg.ConfChan <- nil
	reg.ConfChan <- nil
}

func (r *riskRejectionTest) reject() {
	r.riskMgr.riskRejected <- &swapserverrpc.ServerStaticLoopInRiskRejectedNotification{ // nolint: lll
		SwapHash: r.swapHash[:],
	}
}

func (r *riskRejectionTest) requireInvoiceCanceled() {
	select {
	case hash := <-r.mockLnd.FailInvoiceChannel:
		require.Equal(r.t, r.swapHash, hash)

	case <-r.ctx.Done():
		r.t.Fatalf("invoice was not canceled: %v", r.ctx.Err())
	}
}

func (r *riskRejectionTest) requireLockedForTimeoutSweep() {
	select {
	case transition := <-r.depositMgr.transitionChan:
		require.Equal(r.t, deposit.SweepHtlcTimeout, transition.state)

	case <-r.ctx.Done():
		r.t.Fatalf("deposits were not locked: %v", r.ctx.Err())
	}
}

func (r *riskRejectionTest) requireRunning(resultChan chan fsm.EventType) {
	select {
	case event := <-resultChan:
		r.t.Fatalf("monitor exited with %v: %v", event,
			r.f.LastActionError)

	case <-time.After(200 * time.Millisecond):
	}
}

// requireTimeoutSweep advances past the htlc expiry and checks that the swap
// sweeps the htlc and that the deposits never became available.
func (r *riskRejectionTest) requireTimeoutSweep(
	resultChan chan fsm.EventType) {

	require.NoError(r.t, r.mockLnd.NotifyHeight(r.mockLnd.Height+1))

	require.Equal(r.t, OnSweepHtlcTimeout, r.result(resultChan))

	require.NotContains(r.t, r.depositMgr.states, deposit.Deposited)
	require.True(
		r.t, r.f.loopIn.Deposits[0].IsInState(deposit.SweepHtlcTimeout),
	)
	require.Equal(
		r.t, ConfirmationRiskDecisionRejected,
		r.f.loopIn.ConfirmationRiskDecision,
	)
}

func defaultInvoices(mockLnd *test.LndMockServices) lndclient.InvoicesClient {
	return mockLnd.LndServices.Invoices
}

// TestRiskRejectionAfterHtlcConfSweepsTimeout checks that a risk rejection
// after the htlc confirmed keeps the deposits locked and sweeps the htlc
// timeout path.
func TestRiskRejectionAfterHtlcConfSweepsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	resultChan, reg := r.start(ctx)

	r.confirmHtlc(reg)
	r.reject()
	r.requireInvoiceCanceled()
	r.requireLockedForTimeoutSweep()
	r.requireRunning(resultChan)

	r.requireTimeoutSweep(resultChan)
}

// TestRiskRejectionAfterHtlcConfSurvivesRestart checks that after a restart
// the recovered rejection keeps the deposits locked, even though it is handled
// before the htlc confirmation is delivered again.
func TestRiskRejectionAfterHtlcConfSurvivesRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)

	firstCtx, stop := context.WithCancel(ctx)
	firstResult, reg := r.start(firstCtx)
	r.confirmHtlc(reg)
	r.reject()
	r.requireInvoiceCanceled()
	r.requireLockedForTimeoutSweep()
	stop()
	require.Equal(t, fsm.NoOp, r.result(firstResult))

	// Restart with the rejection recovered from the store and the invoice
	// canceled. The htlc confirmation arrives after the rejection was
	// handled.
	r.mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  r.swapHash,
		State: invoices.ContractCanceled,
	})
	resultChan, reg := r.start(ctx)
	r.requireRunning(resultChan)

	r.confirmHtlc(reg)
	r.requireTimeoutSweep(resultChan)
}

// TestRiskRejectionAfterHtlcConfSettledInvoice checks that the swap finishes
// as paid if the invoice settled before it could be canceled.
func TestRiskRejectionAfterHtlcConfSettledInvoice(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	cancelCalls := make(chan lntypes.Hash, 1)
	release := make(chan struct{})
	r := newRiskRejectionTest(
		t, ctx, func(mockLnd *test.LndMockServices) lndclient.InvoicesClient {
			return &failingCancelInvoices{
				InvoicesClient: mockLnd.LndServices.Invoices,
				cancelCalls:    cancelCalls,
				release:        release,
				err:            errors.New("invoice already settled"),
			}
		},
	)
	resultChan, reg := r.start(ctx)

	r.confirmHtlc(reg)
	r.reject()
	select {
	case <-cancelCalls:
	case <-ctx.Done():
		t.Fatalf("invoice cancel not attempted: %v", ctx.Err())
	}

	r.mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  r.swapHash,
		State: invoices.ContractSettled,
	})
	close(release)

	require.Equal(t, OnPaymentReceived, r.result(resultChan))
	require.Empty(t, r.depositMgr.transitions)
}

// TestRiskRejectionWithoutHtlcConfReleasesDeposits checks that a rejection
// before any htlc confirmation, with the deposits unspent, still fails the
// swap, which unlocks the deposits.
func TestRiskRejectionWithoutHtlcConfReleasesDeposits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	resultChan, _ := r.start(ctx)

	r.reject()
	r.requireInvoiceCanceled()

	require.Equal(t, fsm.OnError, r.result(resultChan))
	require.ErrorContains(
		t, r.f.LastActionError, "server rejected confirmation risk wait",
	)
	require.Empty(t, r.depositMgr.transitions)
	require.Equal(
		t, ConfirmationRiskDecisionRejected,
		r.f.loopIn.ConfirmationRiskDecision,
	)
}

// TestRiskRejectionRecoveredBeforeLock checks a restart after the rejection
// was stored but before the deposits were locked. The htlc spent the deposits,
// so the swap keeps them, and locks and sweeps them once the htlc confirmation
// is delivered again.
func TestRiskRejectionRecoveredBeforeLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	r.recoverRejection()
	r.spendDeposits()

	resultChan, reg := r.start(ctx)
	r.requireInvoiceCanceled()
	r.requireRunning(resultChan)
	require.Empty(t, r.depositMgr.transitions)

	r.confirmHtlc(reg)
	r.requireLockedForTimeoutSweep()
	r.requireTimeoutSweep(resultChan)
	r.requireSpenderLookup()
}

// TestRiskRejectionRetryAfterFailedLock checks that a failed deposit lock
// doesn't release the deposits when the monitor runs again and the rejection
// is replayed before the htlc confirmation.
func TestRiskRejectionRetryAfterFailedLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	r.depositMgr.errs = []error{errors.New("lock failed")}
	r.spendDeposits()

	firstResult, reg := r.start(ctx)
	r.confirmHtlc(reg)
	r.reject()
	r.requireInvoiceCanceled()
	r.requireLockedForTimeoutSweep()
	require.Equal(t, OnRecover, r.result(firstResult))
	require.True(t, r.f.loopIn.Deposits[0].IsInState(deposit.LoopingIn))

	// The monitor runs again. The invoice is canceled, and the cached
	// rejection arrives before the htlc confirmation.
	r.mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  r.swapHash,
		State: invoices.ContractCanceled,
	})
	resultChan, reg := r.start(ctx)
	r.reject()
	r.requireRunning(resultChan)

	r.confirmHtlc(reg)
	r.requireLockedForTimeoutSweep()
	r.requireTimeoutSweep(resultChan)
}

// TestRiskRejectionAfterConfStreamReset checks a rejection that arrives after
// the htlc confirmation subscription failed and before the confirmation is
// delivered again.
func TestRiskRejectionAfterConfStreamReset(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	r.spendDeposits()

	resultChan, reg := r.start(ctx)
	r.confirmHtlc(reg)
	reg.ErrChan <- errors.New("lnd restarting")

	select {
	case reg = <-r.mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc conf not registered again: %v", ctx.Err())
	}

	r.reject()
	r.requireInvoiceCanceled()
	r.requireRunning(resultChan)

	r.confirmHtlc(reg)
	r.requireLockedForTimeoutSweep()
	r.requireTimeoutSweep(resultChan)
}

// TestRiskRejectionPartialLock checks that a rejection finishes a lock that
// stopped after some of the deposits.
func TestRiskRejectionPartialLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	second := &deposit.Deposit{
		Index: 1,
		Value: 100_000,
	}
	second.SetState(deposit.SweepHtlcTimeout)
	r.f.loopIn.Deposits = append(r.f.loopIn.Deposits, second)
	r.depositMgr.active[second.OutPoint] = second
	r.recoverRejection()

	resultChan, reg := r.start(ctx)
	r.requireInvoiceCanceled()
	r.requireLockedForTimeoutSweep()
	r.requireRunning(resultChan)

	r.confirmHtlc(reg)
	r.requireTimeoutSweep(resultChan)
	require.True(t, second.IsInState(deposit.SweepHtlcTimeout))
}

// TestRiskRejectionRestartAfterHtlcExpiry checks a restart after the htlc
// expired with the deposits already locked for the timeout sweep. The swap
// sweeps even if the expired height arrives before the htlc confirmation.
func TestRiskRejectionRestartAfterHtlcExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	r.recoverRejection()
	r.f.loopIn.HtlcCltvExpiry = r.mockLnd.Height - 10
	r.f.loopIn.Deposits[0].SetState(deposit.SweepHtlcTimeout)
	r.mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  r.swapHash,
		State: invoices.ContractCanceled,
	})

	resultChan, _ := r.start(ctx)
	require.Equal(t, OnSweepHtlcTimeout, r.result(resultChan))
	require.NotContains(t, r.depositMgr.states, deposit.Deposited)
}

// TestRiskRejectionSpentDepositsAfterHtlcExpiry checks a restart after the
// htlc expired, with the rejection stored and the deposits spent but not yet
// locked. The expired height arrives before the htlc confirmation, and the
// swap waits for it instead of releasing the spent deposits.
func TestRiskRejectionSpentDepositsAfterHtlcExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	r.recoverRejection()
	r.spendDeposits()
	r.f.loopIn.HtlcCltvExpiry = r.mockLnd.Height - 10

	resultChan, reg := r.start(ctx)
	r.requireInvoiceCanceled()
	r.requireRunning(resultChan)
	require.Empty(t, r.depositMgr.transitions)

	r.confirmHtlc(reg)
	r.requireLockedForTimeoutSweep()
	r.requireTimeoutSweep(resultChan)
}

// TestRiskRejectionStopsPaymentDeadline checks that a rejection after an
// accepted decision stops the payment deadline, which would otherwise release
// the deposits the htlc spent.
func TestRiskRejectionStopsPaymentDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	r.f.loopIn.PaymentTimeoutSeconds = 1
	r.f.loopIn.ConfirmationRiskDecision = ConfirmationRiskDecisionAccepted
	r.f.loopIn.ConfirmationRiskDecisionTime = time.Now()
	r.spendDeposits()

	resultChan, reg := r.start(ctx)
	r.reject()
	r.requireInvoiceCanceled()

	// Wait past the payment deadline.
	select {
	case event := <-resultChan:
		t.Fatalf("monitor exited with %v: %v", event,
			r.f.LastActionError)

	case <-time.After(1500 * time.Millisecond):
	}
	require.Empty(t, r.depositMgr.transitions)

	r.confirmHtlc(reg)
	r.requireLockedForTimeoutSweep()
	r.requireTimeoutSweep(resultChan)
}

// TestRiskRejectionAfterDepositReplacement checks that a rejection after the
// deposit's funding transaction was replaced fails the swap. The deposit is
// gone, but no htlc tx spent it, so no htlc confirmation can come.
func TestRiskRejectionAfterDepositReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	resultChan, _ := r.start(ctx)

	r.replaceDeposits()
	r.reject()
	r.requireInvoiceCanceled()
	r.requireFailed(resultChan)
	r.requireSpenderLookup()
}

// TestRiskRejectionRecoveredAfterDepositReplacement checks a restart with a
// stored rejection after the deposit's funding transaction was replaced. The
// swap fails instead of waiting for an htlc confirmation that can't come.
func TestRiskRejectionRecoveredAfterDepositReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	r.recoverRejection()
	r.replaceDeposits()

	resultChan, _ := r.start(ctx)
	r.requireInvoiceCanceled()
	r.requireFailed(resultChan)
	r.requireSpenderLookup()
}

// TestRiskRejectionRecoveredReplacementAfterHtlcExpiry checks a restart after
// the htlc expiry with a stored rejection and a replaced deposit. The stored
// rejection fails the swap before the monitor handles any block, so the swap
// doesn't wait for the htlc although it already expired.
func TestRiskRejectionRecoveredReplacementAfterHtlcExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	r.recoverRejection()
	r.replaceDeposits()
	r.f.loopIn.HtlcCltvExpiry = r.mockLnd.Height - 10

	resultChan, _ := r.start(ctx)
	r.requireInvoiceCanceled()
	r.requireFailed(resultChan)
}

// TestRiskRejectionSpenderCheckError checks that a failed lookup of the
// deposits' spender retries the monitor instead of releasing the deposits.
func TestRiskRejectionSpenderCheckError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	r.recoverRejection()
	r.txOuts.err = errors.New("list transactions failed")

	resultChan, _ := r.start(ctx)
	r.requireInvoiceCanceled()

	require.Equal(t, OnRecover, r.result(resultChan))
	require.Empty(t, r.depositMgr.transitions)
	require.NotContains(t, r.depositMgr.states, deposit.Deposited)
	r.requireSpenderLookup()
}
