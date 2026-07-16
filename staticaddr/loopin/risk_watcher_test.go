package loopin

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestConfirmationRiskWatcherSubscribeFiltersSwapHash verifies that the watcher
// only emits normalized decisions for the swap it was created for.
func TestConfirmationRiskWatcherSubscribeFiltersSwapHash(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	swapHash := lntypes.Hash{1, 2, 3}
	otherHash := lntypes.Hash{3, 2, 1}
	notificationMgr := &mockNotificationManager{
		riskAccepted: make(
			chan *swapserverrpc.
				ServerStaticLoopInRiskAcceptedNotification, 1,
		),
		riskRejected: make(
			chan *swapserverrpc.
				ServerStaticLoopInRiskRejectedNotification, 1,
		),
	}

	watcher := newConfirmationRiskWatcher(
		&Config{NotificationManager: notificationMgr}, swapHash,
		t.Logf,
	)
	updates, stop := watcher.subscribe(ctx)
	defer stop()

	notificationMgr.riskAccepted <- &swapserverrpc.
		ServerStaticLoopInRiskAcceptedNotification{
		SwapHash: otherHash[:],
	}

	select {
	case update := <-updates:
		t.Fatalf("received wrong-hash risk update: %v", update)

	case <-time.After(100 * time.Millisecond):
	}

	notificationMgr.riskRejected <- &swapserverrpc.
		ServerStaticLoopInRiskRejectedNotification{
		SwapHash: swapHash[:],
	}

	select {
	case update := <-updates:
		require.Equal(t, ConfirmationRiskDecisionRejected,
			update.decision)
		require.Equal(t, "risk rejection", update.reason)

	case <-ctx.Done():
		t.Fatalf("risk update not received: %v", ctx.Err())
	}
}

// TestConfirmationRiskWatcherDecisionTimeRestoration verifies that the watcher
// preserves existing persisted decision timestamps and records missing ones.
func TestConfirmationRiskWatcherDecisionTimeRestoration(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	swapHash := lntypes.Hash{4, 5, 6}
	decisionTime := time.Unix(123, 0).UTC()
	store := &recordingRiskStore{
		mockStore: &mockStore{
			loopIns: map[lntypes.Hash]*StaticAddressLoopIn{
				swapHash: {
					ConfirmationRiskDecision:     ConfirmationRiskDecisionAccepted,
					ConfirmationRiskDecisionTime: decisionTime,
				},
			},
		},
		decisions: make(chan ConfirmationRiskDecision, 1),
	}

	watcher := newConfirmationRiskWatcher(&Config{Store: store}, swapHash,
		t.Logf)
	restoredTime := watcher.decisionTime(
		ctx, ConfirmationRiskDecisionAccepted,
	)
	require.True(t, restoredTime.Equal(decisionTime))

	select {
	case decision := <-store.decisions:
		t.Fatalf("persisted already-recorded decision: %v", decision)

	default:
	}

	store.loopIns[swapHash] = &StaticAddressLoopIn{}
	recordedTime := watcher.decisionTime(
		ctx, ConfirmationRiskDecisionRejected,
	)
	require.False(t, recordedTime.IsZero())

	select {
	case decision := <-store.decisions:
		require.Equal(t, ConfirmationRiskDecisionRejected, decision)

	case <-time.After(time.Second):
		t.Fatal("missing risk decision was not persisted")
	}
	require.Equal(t, ConfirmationRiskDecisionRejected,
		store.loopIns[swapHash].ConfirmationRiskDecision)
	require.True(t, recordedTime.Equal(
		store.loopIns[swapHash].ConfirmationRiskDecisionTime,
	))
}
