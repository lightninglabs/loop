package deposit

import (
	"context"
	"testing"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	// releaseSwapA first locks and releases the deposit.
	releaseSwapA = lntypes.Hash{0xa}

	// releaseSwapB takes the deposit after swap A released it.
	releaseSwapB = lntypes.Hash{0xb}
)

// newReleaseTestManager returns a manager with one confirmed deposit.
func newReleaseTestManager(t *testing.T) (*Manager, *Deposit) {
	t.Helper()

	store := new(mockStore)
	store.On("UpdateDeposit", mock.Anything, mock.Anything).Return(nil)
	m := NewManager(&ManagerConfig{Store: store})

	return m, heldDeposit(m, 1)
}

// releaseCopy releases a store copy of d, like a loop-in does.
func releaseCopy(ctx context.Context, m *Manager, d *Deposit,
	swapHash lntypes.Hash) error {

	return m.ReleaseDepositsForSwap(
		ctx, []*Deposit{{OutPoint: d.OutPoint}}, swapHash,
	)
}

// swapHashOf returns the swap recorded on the deposit.
func swapHashOf(d *Deposit) *lntypes.Hash {
	d.Lock()
	defer d.Unlock()

	return d.SwapHash
}

// TestReleaseDepositsForSwap checks that a second release leaves a deposit
// with the loop-in, withdrawal or channel open that took it.
func TestReleaseDepositsForSwap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		takeOver  func(context.Context, *Manager, *Deposit) error
		wantState fsm.StateType
		wantSwap  lntypes.Hash
	}{
		{
			name: "another loop-in",
			takeOver: func(ctx context.Context, m *Manager,
				d *Deposit) error {

				return m.LockDepositsForSwap(
					ctx, []*Deposit{d}, releaseSwapB,
				)
			},
			wantState: LoopingIn,
			wantSwap:  releaseSwapB,
		},
		{
			name: "withdrawal",
			takeOver: func(ctx context.Context, m *Manager,
				d *Deposit) error {

				return m.TransitionDeposits(
					ctx, []*Deposit{d}, OnWithdrawInitiated,
					Withdrawing,
				)
			},
			wantState: Withdrawing,
			wantSwap:  releaseSwapA,
		},
		{
			name: "channel open",
			takeOver: func(ctx context.Context, m *Manager,
				d *Deposit) error {

				return m.TransitionDeposits(
					ctx, []*Deposit{d}, OnOpeningChannel,
					OpeningChannel,
				)
			},
			wantState: OpeningChannel,
			wantSwap:  releaseSwapA,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			m, d := newReleaseTestManager(t)

			err := m.LockDepositsForSwap(
				ctx, []*Deposit{d}, releaseSwapA,
			)
			require.NoError(t, err)
			require.Equal(t, LoopingIn, d.GetState())
			require.Equal(t, &releaseSwapA, swapHashOf(d))

			// Swap A releases the deposit at its payment deadline.
			require.NoError(t, releaseCopy(ctx, m, d, releaseSwapA))
			require.Equal(t, Deposited, d.GetState())

			require.NoError(t, test.takeOver(ctx, m, d))
			require.Equal(t, test.wantState, d.GetState())

			// Swap A releases again at its htlc expiry.
			require.NoError(t, releaseCopy(ctx, m, d, releaseSwapA))
			require.Equal(t, test.wantState, d.GetState())
			require.Equal(t, &test.wantSwap, swapHashOf(d))
		})
	}
}

// TestReleaseDepositsForSwapInactive checks that releasing an inactive
// deposit succeeds without changing it.
func TestReleaseDepositsForSwapInactive(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	m, d := newReleaseTestManager(t)

	require.NoError(t, m.LockDepositsForSwap(ctx, []*Deposit{d}, releaseSwapA))
	require.NoError(t, releaseCopy(ctx, m, d, releaseSwapA))
	require.NoError(t, m.LockDepositsForSwap(ctx, []*Deposit{d}, releaseSwapB))

	m.removeActiveDeposit(d.OutPoint)

	require.NoError(t, releaseCopy(ctx, m, d, releaseSwapA))
	require.Equal(t, LoopingIn, d.GetState())
}

// TestReleaseDepositsForSwapRecovered checks that only the stored swap can
// release a deposit restored after a restart.
func TestReleaseDepositsForSwapRecovered(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	m, d := newReleaseTestManager(t)

	err := m.TransitionDeposits(
		ctx, []*Deposit{d}, OnLoopInInitiated, LoopingIn,
	)
	require.NoError(t, err)
	d.Lock()
	d.SwapHash = &releaseSwapA
	d.Unlock()

	require.NoError(t, releaseCopy(ctx, m, d, releaseSwapB))
	require.Equal(t, LoopingIn, d.GetState())

	require.NoError(t, releaseCopy(ctx, m, d, releaseSwapA))
	require.Equal(t, Deposited, d.GetState())
}

// TestLockDepositsForSwapRejectsOtherOwner checks that a loop-in can't lock a
// deposit looping in for another swap.
func TestLockDepositsForSwapRejectsOtherOwner(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	m, d := newReleaseTestManager(t)

	require.NoError(t, m.LockDepositsForSwap(ctx, []*Deposit{d}, releaseSwapA))
	require.NoError(t, m.LockDepositsForSwap(ctx, []*Deposit{d}, releaseSwapA))

	err := m.LockDepositsForSwap(ctx, []*Deposit{d}, releaseSwapB)
	require.ErrorContains(t, err, "already looping in")
	require.Equal(t, &releaseSwapA, swapHashOf(d))

	// Nor one looping in without a swap.
	d.Lock()
	d.SwapHash = nil
	d.Unlock()
	err = m.LockDepositsForSwap(ctx, []*Deposit{d}, releaseSwapB)
	require.ErrorContains(t, err, "already looping in")

	d.Lock()
	d.SwapHash = &releaseSwapA
	d.Unlock()
	require.NoError(t, releaseCopy(ctx, m, d, releaseSwapA))
	require.Equal(t, Deposited, d.GetState())
}

// TestLockDepositsForSwapFailedWait checks that a lock whose wait fails still
// records the swap.
func TestLockDepositsForSwapFailedWait(t *testing.T) {
	t.Parallel()

	// Repeat, since the wait races the canceled context.
	failedWaits := 0
	for range 20 {
		m, d := newReleaseTestManager(t)

		canceledCtx, cancel := context.WithCancel(t.Context())
		cancel()
		err := m.LockDepositsForSwap(
			canceledCtx, []*Deposit{d}, releaseSwapA,
		)
		if err != nil {
			require.ErrorContains(t, err, "waiting for state")
			failedWaits++
		}
		require.Equal(t, LoopingIn, d.GetState())
		require.Equal(t, &releaseSwapA, swapHashOf(d))

		require.NoError(t, releaseCopy(t.Context(), m, d, releaseSwapA))
		require.Equal(t, Deposited, d.GetState())
	}
	require.NotZero(t, failedWaits)
}

// TestLockDepositsForSwapPartial checks a lock that fails on a withdrawing
// deposit: the release frees the others and leaves that one alone.
func TestLockDepositsForSwapPartial(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	m, d1 := newReleaseTestManager(t)
	d2 := heldDeposit(m, 2)

	require.NoError(t, m.TransitionDeposits(
		ctx, []*Deposit{d2}, OnWithdrawInitiated, Withdrawing,
	))

	err := m.LockDepositsForSwap(ctx, []*Deposit{d1, d2}, releaseSwapA)
	require.Error(t, err)
	require.Equal(t, LoopingIn, d1.GetState())
	require.Equal(t, &releaseSwapA, swapHashOf(d1))

	err = m.ReleaseDepositsForSwap(
		ctx, []*Deposit{{OutPoint: d1.OutPoint}, {OutPoint: d2.OutPoint}},
		releaseSwapA,
	)
	require.NoError(t, err)
	require.Equal(t, Deposited, d1.GetState())
	require.Equal(t, Withdrawing, d2.GetState())
}

// TestLockDepositsForSwapAfterWithdrawal checks that a release leaves a
// deposit withdrawn before the lock alone.
func TestLockDepositsForSwapAfterWithdrawal(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	m, d := newReleaseTestManager(t)

	require.NoError(t, m.TransitionDeposits(
		ctx, []*Deposit{d}, OnWithdrawInitiated, Withdrawing,
	))

	err := m.LockDepositsForSwap(ctx, []*Deposit{d}, releaseSwapA)
	require.Error(t, err)

	require.NoError(t, releaseCopy(ctx, m, d, releaseSwapA))
	require.Equal(t, Withdrawing, d.GetState())
}
