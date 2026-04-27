package deposit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func expectStableBestBlock(mockChainKit *MockChainKit, height int32) {
	mockChainKit.On(
		"GetBestBlock", mock.Anything,
	).Return(chainhash.Hash{}, height, nil).Twice()
}

func TestReconcileDepositsSerialized(t *testing.T) {
	ctx := context.Background()
	mockLnd := test.NewMockLnd()
	utxo := &lnwallet.Utxo{
		AddressType:   lnwallet.TaprootPubkey,
		Value:         btcutil.Amount(100_000),
		Confirmations: 0,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 1,
		},
	}

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{utxo}, nil)
	mockAddressManager.On(
		"GetStaticAddressParameters", mock.Anything,
	).Return((*address.Parameters)(nil), errors.New("fsm init failed"))

	mockStore := new(mockStore)
	var createCalls atomic.Int32
	createEntered := make(chan struct{})
	releaseCreate := make(chan struct{})
	mockStore.On(
		"CreateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(mock.Arguments) {
		if createCalls.Add(1) == 1 {
			close(createEntered)
		}

		<-releaseCreate
	})

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          mockStore,
		WalletKit:      mockLnd.WalletKit,
		Signer:         mockLnd.Signer,
	})

	var wg sync.WaitGroup
	wg.Add(2)

	errs := make(chan error, 2)
	go func() {
		defer wg.Done()
		errs <- manager.reconcileDeposits(ctx)
	}()

	<-createEntered

	go func() {
		defer wg.Done()
		errs <- manager.reconcileDeposits(ctx)
	}()

	time.Sleep(100 * time.Millisecond)
	close(releaseCreate)
	wg.Wait()
	close(errs)

	var gotErrs []error
	for err := range errs {
		gotErrs = append(gotErrs, err)
	}

	require.EqualValues(t, 1, createCalls.Load())
	require.Len(t, manager.deposits, 1)
	require.Empty(t, manager.activeDeposits)
	require.Len(t, gotErrs, 2)

	var errCount int
	for _, err := range gotErrs {
		if err == nil {
			continue
		}

		errCount++
		require.ErrorContains(t, err, "unable to start new deposit FSM")
	}
	require.Equal(t, 1, errCount)
}

func TestReconcileConfirmedDepositUsesBestBlockHeight(t *testing.T) {
	ctx := context.Background()
	mockLnd := test.NewMockLnd()
	utxo := &lnwallet.Utxo{
		AddressType:   lnwallet.TaprootPubkey,
		Value:         btcutil.Amount(100_000),
		Confirmations: 3,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{8},
			Index: 1,
		},
	}

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{utxo}, nil)
	mockAddressManager.On(
		"GetStaticAddressParameters", mock.Anything,
	).Return((*address.Parameters)(nil), errors.New("fsm init failed"))

	mockChainKit := new(MockChainKit)
	expectStableBestBlock(mockChainKit, 100)

	mockStore := new(mockStore)
	mockStore.On(
		"CreateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(args mock.Arguments) {
		createdDeposit := args.Get(1).(*Deposit)
		require.EqualValues(t, 98, createdDeposit.ConfirmationHeight)
	})

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		ChainKit:       mockChainKit,
		Store:          mockStore,
		WalletKit:      mockLnd.WalletKit,
		Signer:         mockLnd.Signer,
	})

	err := manager.reconcileDeposits(ctx)
	require.ErrorContains(t, err, "unable to start new deposit FSM")
}

// TestReconcileDepositsInvalidatesVanishedUnconfirmedDeposit verifies that a
// single missing ListUnspent observation is reversible, but repeated misses
// still mark the deposit as replaced.
func TestReconcileDepositsInvalidatesVanishedUnconfirmedDeposit(t *testing.T) {
	ctx := context.Background()
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{2},
		Index: 7,
	}

	deposit := &Deposit{
		OutPoint: outpoint,
	}
	deposit.SetState(Deposited)

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{}, nil)

	mockStore := new(mockStore)
	var updateCalls atomic.Int32
	mockStore.On(
		"UpdateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(args mock.Arguments) {
		updateCalls.Add(1)
		updatedDeposit := args.Get(1).(*Deposit)
		require.True(t, updatedDeposit.IsInStateNoLock(Replaced))
	})

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          mockStore,
	})
	manager.deposits[outpoint] = deposit
	fsm := &FSM{
		stopChan: make(chan struct{}),
		quitChan: make(chan struct{}),
	}
	go func() {
		<-fsm.stopChan
		close(fsm.quitChan)
	}()
	manager.activeDeposits[outpoint] = fsm

	// The first miss only increments the consecutive-miss counter.
	require.NoError(t, manager.reconcileDeposits(ctx))
	require.EqualValues(t, 0, updateCalls.Load())
	require.Equal(t, Deposited, deposit.GetState())
	require.Len(t, manager.activeDeposits, 1)

	// The second consecutive miss is strong enough evidence to finalize the
	// record as replaced.
	require.NoError(t, manager.reconcileDeposits(ctx))
	require.EqualValues(t, 1, updateCalls.Load())
	require.Equal(t, Replaced, deposit.GetState())
	require.Empty(t, manager.activeDeposits)
	select {
	case <-fsm.quitChan:

	case <-time.After(time.Second):
		t.Fatal("fsm did not stop after deposit was replaced")
	}
}

// TestReconcileDepositsInvalidatesVanishedConfirmedDeposit verifies that a
// previously confirmed deposit is also marked replaced if it vanishes from the
// wallet view for multiple consecutive observations.
func TestReconcileDepositsInvalidatesVanishedConfirmedDeposit(t *testing.T) {
	ctx := context.Background()
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{9},
		Index: 4,
	}

	deposit := &Deposit{
		OutPoint:           outpoint,
		ConfirmationHeight: 123,
	}
	deposit.SetState(Deposited)

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{}, nil)

	mockStore := new(mockStore)
	var updateCalls atomic.Int32
	mockStore.On(
		"UpdateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(args mock.Arguments) {
		updateCalls.Add(1)
		updatedDeposit := args.Get(1).(*Deposit)
		require.True(t, updatedDeposit.IsInStateNoLock(Replaced))
		require.EqualValues(
			t, 123, updatedDeposit.ConfirmationHeight,
		)
	})

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          mockStore,
	})
	manager.deposits[outpoint] = deposit
	fsm := &FSM{
		stopChan: make(chan struct{}),
		quitChan: make(chan struct{}),
	}
	go func() {
		<-fsm.stopChan
		close(fsm.quitChan)
	}()
	manager.activeDeposits[outpoint] = fsm

	require.NoError(t, manager.reconcileDeposits(ctx))
	require.EqualValues(t, 0, updateCalls.Load())
	require.Equal(t, Deposited, deposit.GetState())
	require.Len(t, manager.activeDeposits, 1)

	require.NoError(t, manager.reconcileDeposits(ctx))
	require.EqualValues(t, 1, updateCalls.Load())
	require.Equal(t, Replaced, deposit.GetState())
	require.Empty(t, manager.activeDeposits)
	select {
	case <-fsm.quitChan:

	case <-time.After(time.Second):
		t.Fatal("fsm did not stop after confirmed deposit was replaced")
	}
}

// TestReconcileDepositsReactivatesReappearedReplacedDeposit verifies that the
// same outpoint can be revived if lnd reports it again after being marked
// replaced.
func TestReconcileDepositsReactivatesReappearedReplacedDeposit(t *testing.T) {
	ctx := context.Background()
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{3},
		Index: 5,
	}

	deposit := &Deposit{
		OutPoint:           outpoint,
		Value:              btcutil.Amount(100_000),
		ConfirmationHeight: 77,
	}
	deposit.SetState(Replaced)

	utxo := &lnwallet.Utxo{
		OutPoint:      outpoint,
		Value:         deposit.Value,
		Confirmations: 0,
	}

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{utxo}, nil)
	mockAddressManager.On(
		"GetStaticAddressParameters", mock.Anything,
	).Return(&address.Parameters{
		ProtocolVersion: version.ProtocolVersion_V0,
	}, nil)
	mockAddressManager.On(
		"GetStaticAddress", mock.Anything,
	).Return((*script.StaticAddress)(nil), nil)

	mockStore := new(mockStore)
	mockStore.On(
		"UpdateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(args mock.Arguments) {
		updatedDeposit := args.Get(1).(*Deposit)
		require.True(t, updatedDeposit.IsInStateNoLock(Deposited))
		require.Zero(t, updatedDeposit.ConfirmationHeight)
	})

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          mockStore,
	})
	manager.deposits[outpoint] = deposit

	// Reconciliation should revive the existing record instead of creating a
	// second deposit entry for the same outpoint.
	require.NoError(t, manager.reconcileDeposits(ctx))
	require.Equal(t, Deposited, deposit.GetState())
	require.Zero(t, deposit.ConfirmationHeight)
	require.Len(t, manager.activeDeposits, 1)
}
