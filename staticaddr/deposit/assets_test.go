package deposit

import (
	"context"
	"encoding/json"
	"sync/atomic"
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

type assetDriverTest struct {
	fundingConfirmed atomic.Bool
	refundConfirmed  atomic.Bool
	builds           atomic.Int32
	publishes        atomic.Int32
	refundBuilds     atomic.Int32
	asset            swapkit.Deposit
}

func (d *assetDriverTest) BuildDeposit(context.Context,
	swapkit.CreateDepositRequest) (swapkit.Deposit, []byte, error) {

	d.builds.Add(1)
	return d.asset, []byte("signed funding"), nil
}

func (d *assetDriverTest) PublishDeposit(_ context.Context,
	asset swapkit.Deposit, tx []byte) (swapkit.Deposit, error) {

	d.publishes.Add(1)
	if !d.fundingConfirmed.Load() {
		return asset, swapkit.ErrPending
	}
	asset.Proof = []byte("confirmed funding")
	asset.ConfirmationHeight, asset.RefundHeight = 10, 110
	return asset, nil
}

func (d *assetDriverTest) BuildDepositRefund(context.Context,
	swapkit.Deposit) ([]byte, error) {

	d.refundBuilds.Add(1)
	return []byte("signed refund"), nil
}

func (d *assetDriverTest) PublishDepositRefund(context.Context,
	swapkit.Deposit, []byte) ([]byte, error) {

	if !d.refundConfirmed.Load() {
		return nil, swapkit.ErrPending
	}
	return []byte("confirmed refund"), nil
}

// TestAssetDepositFSMRestart proves funding and CSV recovery use ordinary
// deposit rows and FSM startup without a separate request or execution store.
func TestAssetDepositFSMRestart(t *testing.T) {
	db := loopdb.NewTestSqliteDB(t)
	store := NewSqlStore(db.BaseDB)
	point := wire.OutPoint{Index: 1}
	contract, err := json.Marshal(map[string]uint32{"CSV": 100})
	require.NoError(t, err)
	driver := &assetDriverTest{asset: swapkit.Deposit{
		Outpoint: point.String(), GroupKey: [33]byte{2}, Amount: 100,
		ValueSat: 5000, Contract: contract,
	}}
	newManager := func() (*Manager, context.CancelFunc) {
		ctx, cancel := context.WithCancel(t.Context())
		address := new(mockAddressManager)
		address.On("GetStaticAddressParameters", mock.Anything).Return(
			&script.Parameters{Expiry: 100}, nil,
		)
		address.On("GetStaticAddress", mock.Anything).Return(
			&script.StaticAddress{}, nil,
		)
		m := NewManager(&ManagerConfig{Store: store,
			AddressManager: address, AssetDriver: driver})
		m.runCtx = ctx
		return m, cancel
	}
	waitState := func(id ID, state string) *Deposit {
		var result *Deposit
		require.Eventually(t, func() bool {
			result, err = store.GetDeposit(t.Context(), id)
			return err == nil && string(result.GetState()) == state
		}, time.Second*5, time.Millisecond*10)
		return result
	}
	first, stopFirst := newManager()
	request := swapkit.CreateDepositRequest{
		RequestID: [32]byte{1}, GroupKey: driver.asset.GroupKey,
		Amount: 100, CSV: 100,
	}
	rpcCtx, cancelRPC := context.WithCancel(t.Context())
	d, err := first.CreateAssetDeposit(rpcCtx, request)
	require.NoError(t, err)
	cancelRPC()
	stored := waitState(d.ID, string(PublishAssetDeposit))
	require.Equal(t, []byte("signed funding"), stored.AssetTx)
	_, ok := first.AllOutpointsActiveDeposits([]wire.OutPoint{point}, Deposited)
	require.False(t, ok)
	stopFirst()
	<-first.activeDeposits[point].quitChan

	priorPublishes := driver.publishes.Load()
	second, stopSecond := newManager()
	require.NoError(t, second.recoverDeposits(second.runCtx))
	require.Eventually(t, func() bool {
		return driver.publishes.Load() > priorPublishes
	}, time.Second*5, time.Millisecond*10)
	// Proof export can lag confirmation. The recovered action must notice
	// its completion without needing another block notification.
	driver.fundingConfirmed.Store(true)
	stored = waitState(d.ID, string(Deposited))
	require.Equal(t, uint64(100), stored.Asset.Amount)
	require.Equal(t, int64(5000), int64(stored.Value))
	require.Empty(t, stored.AssetTx)
	require.NotEmpty(t, stored.Asset.Proof)
	_, err = second.CreateAssetDeposit(t.Context(), request)
	require.NoError(t, err)
	require.EqualValues(t, 1, driver.builds.Load())
	live, err := second.AssetDeposit(t.Context(), point.String())
	require.NoError(t, err)
	require.NoError(t, second.TransitionDeposits(t.Context(), []*Deposit{live},
		OnLoopInInitiated, LoopingIn))
	require.NoError(t, second.activeDeposits[point].SendEvent(
		t.Context(), OnExpiry, nil,
	))
	require.Equal(t, LoopingIn, live.GetState())
	require.Zero(t, driver.refundBuilds.Load())
	require.NoError(t, second.TransitionDeposits(t.Context(), []*Deposit{live},
		fsm.OnError, Deposited))
	second.currentHeight.Store(110)
	id, err := second.RefundAssetDeposit(t.Context(), point.String())
	require.NoError(t, err)
	require.Equal(t, d.ID, id)
	require.Eventually(t, func() bool {
		stored, err = store.GetDeposit(t.Context(), d.ID)
		return err == nil && stored.GetState() == PublishExpirySweep &&
			string(stored.AssetTx) == "signed refund"
	}, time.Second*5, time.Millisecond*10)
	stopSecond()
	<-second.activeDeposits[point].quitChan

	driver.refundConfirmed.Store(true)
	third, stopThird := newManager()
	defer stopThird()
	require.NoError(t, third.recoverDeposits(third.runCtx))
	waitState(d.ID, string(Expired))
	require.EqualValues(t, 1, driver.refundBuilds.Load())

	// Settlement may arrive while the swap's HTLC refund is still pending.
	// Recovery must allow the normal payment action to finalize the deposit.
	late := &Deposit{
		ID: ID{2}, OutPoint: wire.OutPoint{Index: 2},
		Value: 5000, state: SweepHtlcTimeout, Asset: stored.Asset,
		TimeOutSweepPkScript: []byte{},
	}
	require.NoError(t, store.CreateDeposit(t.Context(), late))
	fourth, stopFourth := newManager()
	defer stopFourth()
	require.NoError(t, fourth.recoverDeposits(fourth.runCtx))
	live, err = fourth.AssetDeposit(t.Context(), late.OutPoint.String())
	require.NoError(t, err)
	require.NoError(t, fourth.TransitionDeposits(t.Context(), []*Deposit{live},
		OnLoopedIn, LoopedIn))
	waitState(late.ID, string(LoopedIn))
}
