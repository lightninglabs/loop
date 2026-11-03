package withdraw

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

type sharedInputAddress struct {
	deposit.AddressManager

	input *deposit.Deposit
}

func (*sharedInputAddress) GetStaticAddressParameters(context.Context) (*script.Parameters, error) {
	return &script.Parameters{Expiry: 2016}, nil
}

func (*sharedInputAddress) GetStaticAddress(context.Context) (*script.StaticAddress, error) {
	return &script.StaticAddress{}, nil
}

func (a *sharedInputAddress) ListUnspent(context.Context, int32, int32) ([]*lnwallet.Utxo, error) {
	return []*lnwallet.Utxo{{
		OutPoint: a.input.OutPoint, Value: a.input.Value,
		Confirmations: 501,
	}}, nil
}

// TestWithdrawalClaimSurvivesLoopInCleanup exercises the actual withdrawal
// store and the deposit manager entry points used by static-address Loop In.
func TestWithdrawalClaimSurvivesLoopInCleanup(t *testing.T) {
	ctx := t.Context()
	f := newPersistenceFixture(t, 1)
	input := f.inputs[0]
	outpoints := []wire.OutPoint{input.OutPoint}
	startManager := func() (*deposit.Manager, func()) {
		lnd := test.NewMockLnd()
		m := deposit.NewManager(&deposit.ManagerConfig{
			Store: f.deposits, LightningClient: lnd.Client,
			ChainNotifier:  lnd.ChainNotifier,
			AddressManager: &sharedInputAddress{input: input},
		})
		runCtx, cancel := context.WithCancel(ctx)
		ready, stopped := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(stopped)
			_ = m.Run(runCtx, ready)
		}()
		stop := sync.OnceFunc(func() {
			cancel()
			<-stopped
			lnd.WaitForFinished()
		})
		t.Cleanup(stop)
		select {
		case <-ready:
		case <-stopped:
			t.Fatal("deposit manager stopped before recovery")
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for deposit recovery")
		}
		return m, stop
	}
	manager, stop := startManager()
	data, _ := f.intent(t, f.inputs, 102)
	record := persistenceRecord(t, data, WorkflowSigning, 1)
	store := f.store.WorkflowStore()

	// Queue the competing loop-in after withdrawal admission but before
	// its SQL transaction commits. The manager must recheck the claim.
	loopInResult := make(chan error, 1)
	err := manager.WithWithdrawalLock(ctx, outpoints, func(_ []*deposit.Deposit) error {
		attempting := make(chan struct{})
		go func() {
			close(attempting)
			loopInResult <- manager.TransitionDeposits(ctx, f.inputs,
				deposit.OnLoopInInitiated, deposit.LoopingIn)
		}()
		<-attempting
		return store.Create(ctx, record)
	})
	require.NoError(t, err)
	require.ErrorContains(t, <-loopInResult, "rejects event OnLoopInInitiated in state WithdrawalReserved")
	stop()

	recovered, _ := startManager()
	// Deliver the old loop-in's cleanup using its pre-restart input. Only
	// the durable withdrawal owner can move the deposit out of reservation.
	err = recovered.TransitionDeposits(ctx, f.inputs, fsm.OnError, deposit.Deposited)
	require.ErrorContains(t, err, "rejects event OnError in state WithdrawalReserved")
	_, stillReserved := recovered.AllOutpointsActiveDeposits(outpoints, deposit.WithdrawalReserved)
	require.True(t, stillReserved)
	owner, err := f.db.GetWithdrawalWorkflowClaim(ctx, input.ID[:])
	require.NoError(t, err)
	require.Equal(t, data.ID, owner)
	current, err := store.Load(ctx, data.ID)
	require.NoError(t, err)
	require.Equal(t, record, current)
}
