package flow

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProtocolRecovery(t *testing.T) {
	for _, state := range []State{
		InitHtlcTx, SignHtlcTx, MonitorInvoiceAndHtlcTx,
		SweepHtlcTimeout, PaymentReceived,
	} {
		next, err := Next(ServerAsset, state, OnRecover)
		require.NoError(t, err)
		require.Equal(t, state, next)
	}
	for _, state := range []State{InitHtlcTx, SignHtlcTx} {
		next, err := Next(BitcoinV0, state, OnRecover)
		require.NoError(t, err)
		require.Equal(t, UnlockDeposits, next)
	}
	next, err := Next(BitcoinV0, PaymentReceived, OnRecover)
	require.NoError(t, err)
	require.Equal(t, Succeeded, next)
}

func TestExposedAssetGraphCannotUnlock(t *testing.T) {
	graph := States(ServerAsset)
	seen := make(map[State]bool)
	queue := []State{MonitorInvoiceAndHtlcTx}
	for len(queue) != 0 {
		state := queue[0]
		queue = queue[1:]
		if seen[state] {
			continue
		}
		seen[state] = true
		require.NotContains(t, []State{
			Empty, InitHtlcTx, SignHtlcTx, UnlockDeposits, Failed,
		}, state)
		for _, next := range graph[state] {
			queue = append(queue, next)
		}
	}
	require.True(t, seen[Succeeded])
	require.True(t, seen[HtlcTimeoutSwept])
	for _, event := range []Event{OnError, OnCanceled, OnSwapTimedOut} {
		_, err := Next(ServerAsset, MonitorInvoiceAndHtlcTx, event)
		require.ErrorIs(t, err, ErrTransition)
	}
}
