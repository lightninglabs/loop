package deposit

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// heldDeposit adds a confirmed deposit with a real transition callback.
func heldDeposit(m *Manager, index byte) *Deposit {
	d := &Deposit{
		Hash:               chainhash.Hash{index},
		ConfirmationHeight: 1,
	}
	d.SetState(Deposited)
	sm := &FSM{cfg: m.cfg, deposit: d, stopChan: make(chan struct{})}
	sm.StateMachine = fsm.NewStateMachineWithState(
		sm.DepositStatesV0(), Deposited, DefaultObserverSize,
	)
	sm.ActionEntryFunc = sm.updateDeposit
	m.deposits[d.OutPoint] = d
	m.activeDeposits[d.OutPoint] = sm
	return d
}

// TestHoldActiveDepositsDuringPublication reproduces the wallet removing spent
// inputs before publication returns. Holds preserve the FSMs without changing
// deposit state, and ordinary reconciliation resumes after release.
func TestHoldActiveDepositsDuringPublication(t *testing.T) {
	for _, success := range []bool{false, true} {
		name := "publication fails"
		if success {
			name = "publication succeeds"
		}
		t.Run(name, func(t *testing.T) {
			store := new(mockStore)
			if success {
				store.On("UpdateDeposit", mock.Anything,
					mock.Anything).Return(nil).Twice()
			}
			m := NewManager(&ManagerConfig{Store: store})
			first, second := heldDeposit(m, 1), heldDeposit(m, 2)
			unrelated := heldDeposit(m, 3)
			selected := []*Deposit{first, second}
			release, err := m.HoldActiveDeposits([]wire.OutPoint{
				first.OutPoint, second.OutPoint,
			})
			require.NoError(t, err)
			t.Cleanup(release)
			available, err := m.GetActiveDepositsInState(Deposited)
			require.NoError(t, err)
			require.Equal(t, []*Deposit{unrelated}, available)
			_, selectable := m.AllOutpointsActiveDeposits(
				[]wire.OutPoint{first.OutPoint}, Deposited,
			)
			require.False(t, selectable)
			_, active := m.AllOutpointsActiveDeposits(
				[]wire.OutPoint{first.OutPoint}, fsm.EmptyState,
			)
			require.True(t, active)

			// Publication is still outstanding, but its inputs have
			// vanished from the wallet's current UTXO snapshot.
			require.NoError(t, m.syncActiveDeposits(t.Context(), nil))
			require.NotContains(t, m.activeDeposits, unrelated.OutPoint)
			for _, d := range selected {
				require.Contains(t, m.activeDeposits, d.OutPoint)
				require.Equal(t, Deposited, d.GetState())
				require.Nil(t, d.FinalizedWithdrawalTx)
			}

			if success {
				for _, d := range selected {
					d.FinalizedWithdrawalTx = wire.NewMsgTx(2)
				}
				require.NoError(t, m.TransitionDeposits(
					t.Context(), selected, OnWithdrawInitiated,
					Withdrawing,
				))
			}
			release()
			require.Empty(t, m.activeDepositHolds)
			if !success {
				_, selectable = m.AllOutpointsActiveDeposits(
					[]wire.OutPoint{first.OutPoint}, Deposited,
				)
				require.True(t, selectable)
			}
			require.NoError(t, m.syncActiveDeposits(t.Context(), nil))
			if success {
				require.Len(t, m.activeDeposits, 2)
			} else {
				require.Empty(t, m.activeDeposits)
			}
			store.AssertExpectations(t)
		})
	}
}

// TestHoldActiveDepositsDefersExpiry ensures retaining spent inputs does not
// start a competing expiry sweep while publication is pending. Other deposits
// still receive blocks, and expiry resumes after a failed publication releases
// its hold and the wallet reports the input as unspent again.
func TestHoldActiveDepositsDefersExpiry(t *testing.T) {
	store := new(mockStore)
	store.On("UpdateDeposit", mock.Anything, mock.Anything).Return(nil).Once()
	m := NewManager(&ManagerConfig{Store: store})
	d := heldDeposit(m, 1)
	unrelated := heldDeposit(m, 2)
	feeBump := heldDeposit(m, 3)
	feeBump.SetState(Withdrawing)
	for _, sm := range m.activeDeposits {
		sm.blockNtfnChan = make(chan uint32, 1)
	}
	sm := m.activeDeposits[d.OutPoint]
	sm.params = &script.Parameters{Expiry: 5}
	// Exercise the real expiry transition and persistence callback without
	// constructing and publishing a sweep transaction.
	sm.States[PublishExpirySweep] = fsm.State{Action: fsm.NoOpAction}

	release, err := m.HoldActiveDeposits([]wire.OutPoint{
		d.OutPoint, feeBump.OutPoint,
	})
	require.NoError(t, err)
	t.Cleanup(release)
	utxos := []*lnwallet.Utxo{{OutPoint: unrelated.OutPoint}}
	require.NoError(t, m.syncActiveDeposits(t.Context(), utxos))
	require.Contains(t, m.activeDeposits, d.OutPoint)
	require.NoError(t, m.notifyActiveDeposits(t.Context(), 6))
	require.Empty(t, sm.blockNtfnChan)
	require.Equal(t, Deposited, d.GetState())
	for _, other := range []*Deposit{unrelated, feeBump} {
		notifications := m.activeDeposits[other.OutPoint].blockNtfnChan
		require.Len(t, notifications, 1)
		require.Equal(t, uint32(6), <-notifications)
	}

	release()
	utxos = append(utxos, &lnwallet.Utxo{OutPoint: d.OutPoint})
	require.NoError(t, m.syncActiveDeposits(t.Context(), utxos))
	require.NoError(t, m.notifyActiveDeposits(t.Context(), 7))
	require.Len(t, sm.blockNtfnChan, 1)
	sm.handleBlockNotification(t.Context(), <-sm.blockNtfnChan)
	require.NoError(t, sm.DefaultObserver.WaitForState(
		t.Context(), time.Second, PublishExpirySweep,
	))
	require.Equal(t, PublishExpirySweep, d.GetState())
	store.AssertExpectations(t)
}

// TestHoldActiveDepositsLifetime covers failed acquisition, overlapping holds,
// caller-owned slices, and repeated release without leaking or losing a hold.
func TestHoldActiveDepositsLifetime(t *testing.T) {
	m := NewManager(&ManagerConfig{Store: new(mockStore)})
	d := heldDeposit(m, 1)
	missing := wire.OutPoint{Hash: chainhash.Hash{2}}
	for _, points := range [][]wire.OutPoint{
		{d.OutPoint, missing}, {d.OutPoint, d.OutPoint},
	} {
		release, err := m.HoldActiveDeposits(points)
		require.Error(t, err)
		require.Nil(t, release)
		require.Empty(t, m.activeDepositHolds)
	}

	points := []wire.OutPoint{d.OutPoint}
	first, err := m.HoldActiveDeposits(points)
	require.NoError(t, err)
	t.Cleanup(first)
	second, err := m.HoldActiveDeposits(points)
	require.NoError(t, err)
	t.Cleanup(second)
	points[0] = missing
	first()
	first()
	require.NoError(t, m.syncActiveDeposits(t.Context(), nil))
	require.Contains(t, m.activeDeposits, d.OutPoint)
	second()
	require.NoError(t, m.syncActiveDeposits(t.Context(), nil))
	require.Empty(t, m.activeDeposits)
	require.Empty(t, m.activeDepositHolds)
}
