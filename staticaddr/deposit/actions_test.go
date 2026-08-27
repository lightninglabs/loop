package deposit

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
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

func TestWaitForExpirySweepActionRegistersByScriptOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	timeoutPkScript := []byte{0x51, 0x20, 0x01}
	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	errChan := make(chan error, 1)

	chainNotifier := &MockChainNotifier{}
	chainNotifier.On(
		"RegisterConfirmationsNtfn",
		mock.Anything,
		mock.MatchedBy(func(txid *chainhash.Hash) bool {
			return txid == nil
		}),
		timeoutPkScript,
		int32(DefaultConfTarget),
		int32(42),
	).Return(confChan, errChan, nil).Once()

	depositFSM := &FSM{
		cfg: &ManagerConfig{
			ChainNotifier: chainNotifier,
		},
		deposit: &Deposit{
			ConfirmationHeight:   42,
			ExpirySweepTxid:      chainhash.Hash{9},
			TimeOutSweepPkScript: timeoutPkScript,
		},
	}

	confirmedTx := wire.NewMsgTx(2)
	confirmedTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: timeoutPkScript,
	})
	confChan <- &chainntnfs.TxConfirmation{Tx: confirmedTx}

	event := depositFSM.WaitForExpirySweepAction(ctx, nil)
	require.Equal(t, OnExpirySwept, event)
	require.Equal(t, confirmedTx.TxHash(), depositFSM.deposit.ExpirySweepTxid)
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
