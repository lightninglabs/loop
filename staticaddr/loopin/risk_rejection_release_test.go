package loopin

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// ownedDepositManager adds the real manager's ownership rule to the
// recording manager: a release only moves deposits that are LoopingIn for
// the releasing swap.
type ownedDepositManager struct {
	*recordingDepositManager

	owner map[wire.OutPoint]lntypes.Hash
}

// LockDepositsForSwap records the swap as the owner of the deposits.
func (o *ownedDepositManager) LockDepositsForSwap(ctx context.Context,
	deposits []*deposit.Deposit, swapHash lntypes.Hash) error {

	for _, d := range deposits {
		o.owner[d.OutPoint] = swapHash
	}

	return o.recordingDepositManager.LockDepositsForSwap(
		ctx, deposits, swapHash,
	)
}

// ReleaseDepositsForSwap releases only the deposits that are looping in for
// the swap.
func (o *ownedDepositManager) ReleaseDepositsForSwap(_ context.Context,
	deposits []*deposit.Deposit, swapHash lntypes.Hash) error {

	o.releasedFor = append(o.releasedFor, swapHash)
	for _, d := range deposits {
		active := o.active[d.OutPoint]
		if active == nil || !active.IsInState(deposit.LoopingIn) ||
			o.owner[d.OutPoint] != swapHash {

			continue
		}
		active.SetState(deposit.Deposited)
	}

	return nil
}

// TestRiskRejectionAfterDeadlineKeepsTakenDeposit checks that a risk
// rejection after the payment deadline doesn't release a deposit that was
// taken after the deadline released it. The server accepts the risk wait and
// lets the payment deadline pass, so the swap releases deposit D. The user
// withdraws D, then the server rejects. No htlc tx spent D, so the swap
// fails and releases its deposits again, which must leave D withdrawing.
func TestRiskRejectionAfterDeadlineKeepsTakenDeposit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	r := newRiskRejectionTest(t, ctx, defaultInvoices)
	r.f.loopIn.PaymentTimeoutSeconds = 1
	r.f.loopIn.ConfirmationRiskDecision = ConfirmationRiskDecisionAccepted
	r.f.loopIn.ConfirmationRiskDecisionTime = time.Now()

	d := r.f.loopIn.Deposits[0]
	mgr := &ownedDepositManager{
		recordingDepositManager: r.depositMgr,
		owner: map[wire.OutPoint]lntypes.Hash{
			d.OutPoint: r.swapHash,
		},
	}
	r.f.cfg.DepositManager = mgr

	resultChan, _ := r.start(ctx)

	// Payment deadline: invoice canceled, D released.
	r.requireInvoiceCanceled()
	require.Eventually(t, func() bool {
		return d.IsInState(deposit.Deposited)
	}, 5*time.Second, 10*time.Millisecond)

	// The user withdraws D. The withdrawal tx spends it, so lnd no longer
	// lists it, and no htlc tx spent it.
	d.SetState(deposit.Withdrawing)
	r.replaceDeposits()

	// The server now rejects the risk wait.
	r.reject()
	require.Equal(t, fsm.OnError, r.result(resultChan))
	r.requireSpenderLookup()

	// The FSM moves to UnlockDeposits.
	require.Equal(t, fsm.OnError, r.f.UnlockDepositsAction(ctx, nil))
	require.True(t, d.IsInState(deposit.Withdrawing),
		"withdrawn deposit reset to %v", d.GetState())
	require.Equal(
		t, []lntypes.Hash{r.swapHash, r.swapHash}, mgr.releasedFor,
	)
}
