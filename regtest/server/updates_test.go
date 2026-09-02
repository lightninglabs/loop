package server

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
)

func TestUpdateHubReplayAndFinish(t *testing.T) {
	t.Parallel()

	hub := newUpdateHub()
	hub.publish(swapserverrpc.ServerSwapState_SERVER_INITIATED)

	subscriber := hub.subscribe()
	require.Len(t, subscriber.history, 1)
	require.Equal(t, swapserverrpc.ServerSwapState_SERVER_INITIATED,
		subscriber.history[0].state)

	hub.publish(swapserverrpc.ServerSwapState_SERVER_HTLC_PUBLISHED)
	select {
	case update := <-subscriber.updates:
		require.Equal(t,
			swapserverrpc.ServerSwapState_SERVER_HTLC_PUBLISHED,
			update.state)
	case <-time.After(time.Second):
		t.Fatal("live update not delivered")
	}

	hub.finish(swapserverrpc.ServerSwapState_SERVER_SUCCESS)
	select {
	case update := <-subscriber.updates:
		require.Equal(t, swapserverrpc.ServerSwapState_SERVER_SUCCESS,
			update.state)
	case <-time.After(time.Second):
		t.Fatal("terminal update not delivered")
	}

	_, ok := <-subscriber.updates
	require.False(t, ok)

	late := hub.subscribe()
	require.True(t, late.done)
	require.Len(t, late.history, 3)
	require.Equal(t, swapserverrpc.ServerSwapState_SERVER_SUCCESS,
		late.history[2].state)
}

func TestNotificationHubCancellation(t *testing.T) {
	t.Parallel()

	hub := newNotificationHub()
	want := &swapserverrpc.SubscribeNotificationsResponse{}
	hub.publish(want)

	ctx, cancel := context.WithCancel(context.Background())
	updates := hub.subscribe(ctx)
	select {
	case got := <-updates:
		require.Same(t, want, got)
	case <-time.After(time.Second):
		t.Fatal("notification not delivered")
	}

	cancel()
	select {
	case _, ok := <-updates:
		require.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("notification subscription not closed")
	}
}
