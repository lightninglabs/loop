package deposit

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestAssetDepositFSMRestart checks that ordinary recovery retains the saved
// transaction instead of rebuilding it. A damaged native package must remain
// pending without calling either wallet signer; the process suite covers
// successful publication and proof recovery through the SDK and tapd.
func TestAssetDepositFSMRestart(t *testing.T) {
	for _, state := range []fsm.StateType{
		PublishAssetDeposit, PublishExpirySweep,
	} {
		t.Run(string(state), func(t *testing.T) {
			store := NewSqlStore(loopdb.NewTestSqliteDB(t).BaseDB)
			point := wire.OutPoint{Index: 1}
			saved := &Deposit{
				ID: ID{1}, OutPoint: point, Value: 5000, state: state,
				Asset: &swapkit.Deposit{
					Outpoint: point.String(), Amount: 100,
					ValueSat: 5000, GroupKey: [33]byte{2},
				},
				AssetTx:              []byte("damaged native SDK package"),
				TimeOutSweepPkScript: []byte{},
			}
			require.NoError(t, store.CreateDeposit(t.Context(), saved))
			restore := func() {
				ctx, cancel := context.WithCancel(t.Context())
				m := assetRecoveryManager(t, store)
				m.runCtx = ctx
				require.NoError(t, m.recoverDeposits(ctx))
				sm := m.activeDeposits[point]
				require.NotNil(t, sm)
				require.NoError(t, sm.DefaultObserver.WaitForState(
					ctx, DefaultTransitionTimeout, state,
				))
				cancel()
				<-sm.quitChan
				stored, err := store.GetDeposit(t.Context(), saved.ID)
				require.NoError(t, err)
				require.Equal(t, state, stored.GetState())
				require.Equal(t, saved.AssetTx, stored.AssetTx)
				require.Equal(t, saved.Asset, stored.Asset)
			}
			for range 2 {
				restore()
			}
		})
	}
}

func assetRecoveryManager(t *testing.T, store Store) *Manager {
	t.Helper()
	address := new(mockAddressManager)
	address.On("GetStaticAddressParameters", mock.Anything).Return(
		&script.Parameters{Expiry: 100}, nil,
	)
	address.On("GetStaticAddress", mock.Anything).Return(
		&script.StaticAddress{}, nil,
	)
	return NewManager(&ManagerConfig{
		Store: store, AddressManager: address, Asset: &AssetConfig{},
	})
}

// TestAssetDepositRecoveryReservation keeps the normal swap reservation and
// allows a recovered timeout deposit to complete when its invoice settles.
func TestAssetDepositRecoveryReservation(t *testing.T) {
	store := NewSqlStore(loopdb.NewTestSqliteDB(t).BaseDB)
	for i, state := range []fsm.StateType{LoopingIn, SweepHtlcTimeout} {
		d := &Deposit{
			ID: ID{byte(i + 1)}, OutPoint: wire.OutPoint{Index: uint32(i)},
			Value: 5000, state: state,
			Asset:                &swapkit.Deposit{Amount: 100, RefundHeight: 110},
			TimeOutSweepPkScript: []byte{},
		}
		require.NoError(t, store.CreateDeposit(t.Context(), d))
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := assetRecoveryManager(t, store)
	m.runCtx = ctx
	require.NoError(t, m.recoverDeposits(ctx))
	m.currentHeight.Store(110)
	reserved := wire.OutPoint{Index: 0}
	_, err := m.RefundAssetDeposit(t.Context(), reserved.String())
	require.Error(t, err)
	require.Equal(t, LoopingIn, m.activeDeposits[reserved].deposit.GetState())

	late := wire.OutPoint{Index: 1}
	d, err := m.AssetDeposit(t.Context(), late.String())
	require.NoError(t, err)
	require.NoError(t, m.TransitionDeposits(t.Context(), []*Deposit{d},
		OnLoopedIn, LoopedIn))
	require.Eventually(t, func() bool {
		stored, err := store.GetDeposit(t.Context(), d.ID)
		return err == nil && stored.GetState() == LoopedIn
	}, time.Second*5, time.Millisecond*10)
}

// TestAssetDepositsExcludedFromBitcoinViews checks both startup and recovered
// cache views without removing native records from the shared deposit store.
func TestAssetDepositsExcludedFromBitcoinViews(t *testing.T) {
	store := NewSqlStore(loopdb.NewTestSqliteDB(t).BaseDB)
	manager := NewManager(&ManagerConfig{Store: store})
	bitcoin := &Deposit{
		ID: ID{1}, OutPoint: wire.OutPoint{Index: 1}, Value: 5000,
		state: Deposited, TimeOutSweepPkScript: []byte{},
	}
	finalized := &Deposit{
		ID: ID{2}, OutPoint: wire.OutPoint{Index: 2}, Value: 6000,
		state: LoopedIn, TimeOutSweepPkScript: []byte{},
	}
	for _, d := range []*Deposit{bitcoin, finalized} {
		require.NoError(t, store.CreateDeposit(t.Context(), d))
	}
	assetStates := []fsm.StateType{
		PublishAssetDeposit, Deposited, LoopingIn, LoopedIn, Expired,
	}
	for i, state := range assetStates {
		d := &Deposit{
			ID:       ID{byte(i + 10)},
			OutPoint: wire.OutPoint{Index: uint32(i + 10)},
			Value:    1000, state: state, Asset: &swapkit.Deposit{Amount: 100},
			TimeOutSweepPkScript: []byte{},
		}
		require.NoError(t, store.CreateDeposit(t.Context(), d))
	}
	for _, recovered := range []bool{false, true} {
		if recovered {
			manager.deposits[bitcoin.OutPoint] = bitcoin
			manager.activeDeposits[bitcoin.OutPoint] = &FSM{}
		}
		visible, err := manager.GetVisibleDeposits(t.Context())
		require.NoError(t, err)
		require.Len(t, visible, 2)
		var ids []ID
		for _, d := range visible {
			require.Nil(t, d.Asset)
			ids = append(ids, d.ID)
		}
		require.ElementsMatch(t, []ID{bitcoin.ID, finalized.ID}, ids)
		all, err := manager.GetAllDeposits(t.Context())
		require.NoError(t, err)
		require.Len(t, all, 2+len(assetStates))
	}

	// Existing stale Bitcoin filtering still applies after cache recovery.
	delete(manager.activeDeposits, bitcoin.OutPoint)
	visible, err := manager.GetVisibleDeposits(t.Context())
	require.NoError(t, err)
	require.Len(t, visible, 1)
	require.Equal(t, finalized.ID, visible[0].ID)
}
