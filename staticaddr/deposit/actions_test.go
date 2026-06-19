package deposit

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
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
