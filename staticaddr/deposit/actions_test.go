package deposit

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestFinalizeDepositActionDoesNotBlock ensures the final cleanup notification
// does not block the withdrawal completion path while the manager loop is busy.
func TestFinalizeDepositActionDoesNotBlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{1},
		Index: 1,
	}

	depositFSM := &FSM{
		deposit: &Deposit{
			OutPoint: outpoint,
		},
		quitChan:             make(chan struct{}),
		finalizedDepositChan: make(chan wire.OutPoint),
	}

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- depositFSM.FinalizeDepositAction(ctx, nil)
	}()

	select {
	case result := <-resultChan:
		require.Equal(t, fsm.NoOp, result)

	case <-time.After(100 * time.Millisecond):
		t.Fatal("FinalizeDepositAction blocked on manager cleanup")
	}

	select {
	case gotOutpoint := <-depositFSM.finalizedDepositChan:
		require.Equal(t, outpoint, gotOutpoint)

	case <-time.After(time.Second):
		t.Fatal("finalization cleanup notification was not delivered")
	}
}

func TestWaitForExpirySweepActionTracksOutpointSpender(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	depositOutpoint := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
	depositPkScript := []byte{0x51, 0x20, 0x00}
	timeoutPkScript := []byte{0x51, 0x20, 0x01}
	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	spendErrChan := make(chan error, 1)
	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	confErrChan := make(chan error, 1)

	chainNotifier := &MockChainNotifier{}
	chainNotifier.On(
		"RegisterSpendNtfn",
		mock.Anything,
		mock.MatchedBy(func(outpoint *wire.OutPoint) bool {
			return outpoint != nil && *outpoint == depositOutpoint
		}),
		depositPkScript,
		int32(42),
	).Return(spendChan, spendErrChan, nil).Once()

	spendingTx := wire.NewMsgTx(2)
	spendingTx.AddTxIn(&wire.TxIn{PreviousOutPoint: depositOutpoint})
	spendingTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: timeoutPkScript,
	})
	spendingTxID := spendingTx.TxHash()

	chainNotifier.On(
		"RegisterConfirmationsNtfn",
		mock.Anything,
		mock.MatchedBy(func(txid *chainhash.Hash) bool {
			return txid != nil && *txid == spendingTxID
		}),
		timeoutPkScript,
		int32(DefaultConfTarget),
		int32(50),
	).Return(confChan, confErrChan, nil).Once()

	depositFSM := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &ManagerConfig{
			ChainNotifier: chainNotifier,
		},
		deposit: &Deposit{
			OutPoint:             depositOutpoint,
			ConfirmationHeight:   42,
			ExpirySweepTxid:      chainhash.Hash{9},
			TimeOutSweepPkScript: timeoutPkScript,
			AddressParams: &address.Parameters{
				PkScript: depositPkScript,
			},
		},
	}

	spendChan <- &chainntnfs.SpendDetail{
		SpendingTx:     spendingTx,
		SpendingHeight: 50,
	}
	confChan <- &chainntnfs.TxConfirmation{Tx: spendingTx}

	event := depositFSM.WaitForExpirySweepAction(ctx, nil)
	require.Equal(t, OnExpirySwept, event)
	require.Equal(t, spendingTxID, depositFSM.deposit.ExpirySweepTxid)
	chainNotifier.AssertExpectations(t)
}

func TestWaitForExpirySweepActionRejectsInvalidSpend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	depositOutpoint := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
	depositPkScript := []byte{0x51, 0x20, 0x00}
	timeoutPkScript := []byte{0x51, 0x20, 0x01}
	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	spendErrChan := make(chan error, 1)

	chainNotifier := &MockChainNotifier{}
	chainNotifier.On(
		"RegisterSpendNtfn", mock.Anything, mock.Anything,
		depositPkScript, int32(42),
	).Return(spendChan, spendErrChan, nil).Once()

	depositFSM := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &ManagerConfig{
			ChainNotifier: chainNotifier,
		},
		deposit: &Deposit{
			OutPoint:             depositOutpoint,
			ConfirmationHeight:   42,
			TimeOutSweepPkScript: timeoutPkScript,
			AddressParams: &address.Parameters{
				PkScript: depositPkScript,
			},
		},
	}

	// A transaction that merely pays the timeout script must not be treated
	// as the deposit's expiry sweep.
	unrelatedTx := wire.NewMsgTx(2)
	unrelatedTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{2}},
	})
	unrelatedTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: timeoutPkScript,
	})
	spendChan <- &chainntnfs.SpendDetail{SpendingTx: unrelatedTx}

	event := depositFSM.WaitForExpirySweepAction(ctx, nil)
	require.Equal(t, fsm.OnError, event)
	require.Zero(t, depositFSM.deposit.ExpirySweepTxid)
	chainNotifier.AssertNotCalled(t, "RegisterConfirmationsNtfn")
	chainNotifier.AssertExpectations(t)
}

func TestWaitForExpirySweepActionRejectsMissingConfirmation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	depositOutpoint := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
	depositPkScript := []byte{0x51, 0x20, 0x00}
	timeoutPkScript := []byte{0x51, 0x20, 0x01}
	spendingTx := wire.NewMsgTx(2)
	spendingTx.AddTxIn(&wire.TxIn{PreviousOutPoint: depositOutpoint})
	spendingTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: timeoutPkScript,
	})

	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	spendErrChan := make(chan error, 1)
	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	confErrChan := make(chan error, 1)
	spendChan <- &chainntnfs.SpendDetail{
		SpendingTx:     spendingTx,
		SpendingHeight: 50,
	}
	confChan <- nil

	chainNotifier := &MockChainNotifier{}
	chainNotifier.On(
		"RegisterSpendNtfn", mock.Anything, mock.Anything,
		depositPkScript, int32(42),
	).Return(spendChan, spendErrChan, nil).Once()
	chainNotifier.On(
		"RegisterConfirmationsNtfn", mock.Anything, mock.Anything,
		timeoutPkScript, int32(DefaultConfTarget), int32(50),
	).Return(confChan, confErrChan, nil).Once()

	depositFSM := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &ManagerConfig{
			ChainNotifier: chainNotifier,
		},
		deposit: &Deposit{
			OutPoint:             depositOutpoint,
			ConfirmationHeight:   42,
			TimeOutSweepPkScript: timeoutPkScript,
			AddressParams: &address.Parameters{
				PkScript: depositPkScript,
			},
		},
	}

	event := depositFSM.WaitForExpirySweepAction(ctx, nil)
	require.Equal(t, fsm.OnError, event)
	require.ErrorContains(
		t, depositFSM.LastActionError, "confirmation missing transaction",
	)
	require.Zero(t, depositFSM.deposit.ExpirySweepTxid)
	chainNotifier.AssertExpectations(t)
}

// TestFinalizeDepositActionIgnoresRequestCancellation ensures the cleanup
// notification is tied to the FSM lifetime, not the caller's request context.
func TestFinalizeDepositActionIgnoresRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	quitChan := make(chan struct{})
	defer close(quitChan)

	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{2},
		Index: 2,
	}

	depositFSM := &FSM{
		deposit: &Deposit{
			OutPoint: outpoint,
		},
		quitChan:             quitChan,
		finalizedDepositChan: make(chan wire.OutPoint),
	}

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- depositFSM.FinalizeDepositAction(ctx, nil)
	}()

	select {
	case result := <-resultChan:
		require.Equal(t, fsm.NoOp, result)

	case <-time.After(100 * time.Millisecond):
		t.Fatal("FinalizeDepositAction blocked on manager cleanup")
	}

	cancel()

	select {
	case gotOutpoint := <-depositFSM.finalizedDepositChan:
		require.Equal(t, outpoint, gotOutpoint)

	case <-time.After(time.Second):
		t.Fatal("finalization cleanup notification was dropped after " +
			"request cancellation")
	}
}

// TestFinalizeDepositActionIgnoresCanceledContext ensures the final cleanup
// notification is still queued even if the caller's context is already done.
func TestFinalizeDepositActionIgnoresCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	quitChan := make(chan struct{})
	defer close(quitChan)

	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{3},
		Index: 3,
	}

	depositFSM := &FSM{
		deposit: &Deposit{
			OutPoint: outpoint,
		},
		quitChan:             quitChan,
		finalizedDepositChan: make(chan wire.OutPoint),
	}

	result := depositFSM.FinalizeDepositAction(ctx, nil)
	require.Equal(t, fsm.NoOp, result)

	select {
	case gotOutpoint := <-depositFSM.finalizedDepositChan:
		require.Equal(t, outpoint, gotOutpoint)

	case <-time.After(time.Second):
		t.Fatal("finalization cleanup notification was dropped for " +
			"an already-canceled request context")
	}
}
