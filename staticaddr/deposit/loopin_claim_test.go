package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestLoopInClaimOwnership protects a retry's pre-persistence claim from an
// older swap's cleanup, including when the deposit has no durable swap hash.
func TestLoopInClaimOwnership(t *testing.T) {
	h := newManagerTestContext(t)
	m := h.manager
	params, err := h.mockAddressManager.GetStaticAddressParameters(t.Context())
	require.NoError(t, err)
	d := &Deposit{
		Hash:          chainhash.Hash{31},
		AddressParams: params,
	}
	d.SetState(Deposited)
	sm, err := NewFSM(t.Context(), d, m.cfg, m.finalizedDepositChan, true)
	require.NoError(t, err)
	m.activeDeposits[d.OutPoint] = sm
	m.deposits[d.OutPoint] = d
	deposits := []*Deposit{d}
	a, b := lntypes.Hash{1}, lntypes.Hash{2}
	require.NoError(t, m.LockDepositsForLoopIn(t.Context(), deposits, a))
	require.Nil(t, d.SwapHash)
	require.Equal(t, LoopingIn, d.GetState())
	require.NoError(t, m.ReleaseLoopInDeposits(t.Context(), deposits, a))
	require.NoError(t, m.LockDepositsForLoopIn(t.Context(), deposits, b))

	// B is not persisted yet. A repeated cleanup must neither release B nor
	// change its owner, even though both operations saw a nil durable hash.
	require.NoError(t, m.ReleaseLoopInDeposits(t.Context(), deposits, a))
	require.Equal(t, LoopingIn, d.GetState())
	require.Equal(t, b, m.loopInOwners[d.OutPoint])
	require.Error(t, m.LockDepositsForLoopIn(t.Context(), deposits, a))
	require.NoError(t, m.ReleaseLoopInDeposits(t.Context(), deposits, b))
	require.Equal(t, Deposited, d.GetState())
	require.Empty(t, m.loopInOwners)

	// An already reused deposit in another operation must stay untouched.
	require.NoError(t, m.LockDepositsForLoopIn(t.Context(), deposits, a))
	require.NoError(t, m.ReleaseLoopInDeposits(t.Context(), deposits, a))
	require.NoError(t, m.TransitionDeposits(t.Context(), deposits,
		OnWithdrawInitiated, Withdrawing))
	require.NoError(t, m.ReleaseLoopInDeposits(t.Context(), deposits, a))
	require.Equal(t, Withdrawing, d.GetState())
}
