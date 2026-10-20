package flow

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAssetExposureSafety ensures generic failure and time cannot release an
// exposed deposit or invoke a Bitcoin-only transaction path.
func TestAssetExposureSafety(t *testing.T) {
	state, err := Next(ServerAsset, Deposited, OnLoopInInitiated)
	require.NoError(t, err)
	require.Equal(t, LoopingIn, state)
	next, err := Next(ServerAsset, state, OnError)
	require.NoError(t, err)
	require.Equal(t, Deposited, next)
	state, err = Next(ServerAsset, state, OnClaimExposed)
	require.NoError(t, err)
	require.Equal(t, AssetClaimExposed, state)
	for _, protected := range []State{state, SweepHtlcTimeout} {
		events := []Event{OnRecover, OnError, OnExpiry}
		for _, event := range events {
			next, err := Next(ServerAsset, protected, event)
			require.NoError(t, err)
			require.Equal(t, protected, next)
			require.False(t, IsFinal(next))
		}
		for _, event := range []Event{
			OnWithdrawInitiated, OnOpeningChannel,
			OnExpiryPublished,
		} {
			_, err := Next(ServerAsset, protected, event)
			require.ErrorIs(t, err, ErrTransition)
		}
	}
	state, err = Next(ServerAsset, state, OnSweepingHtlcTimeout)
	require.NoError(t, err)
	for event, expected := range map[Event]State{
		OnLoopedIn: LoopedIn, OnHtlcTimeoutSwept: HtlcTimeoutSwept,
	} {
		next, err := Next(ServerAsset, state, event)
		require.NoError(t, err)
		require.Equal(t, expected, next)
		require.True(t, IsFinal(next))
	}
}

// TestAssetGraphRejectsBitcoinSweeps ensures no asset state reaches a path
// that can consume the anchor without generating an asset transition proof.
func TestAssetGraphRejectsBitcoinSweeps(t *testing.T) {
	for _, events := range States(ServerAsset) {
		for _, next := range events {
			require.NotContains(t, []State{
				PublishExpirySweep, WaitForExpirySweep, Expired,
				Withdrawing, Withdrawn, OpeningChannel,
				ChannelPublished,
			}, next)
		}
	}
	_, err := Next(Protocol(255), Deposited, OnLoopInInitiated)
	require.ErrorIs(t, err, ErrTransition)
}

// TestIndependentGraphs keeps protocol-specific policy edits isolated.
func TestIndependentGraphs(t *testing.T) {
	asset := States(ServerAsset)
	asset[LoopingIn][OnError] = LoopedIn
	next, err := Next(ServerAsset, LoopingIn, OnError)
	require.NoError(t, err)
	require.Equal(t, Deposited, next)
	next, err = Next(Bitcoin, LoopingIn, OnExpiry)
	require.NoError(t, err)
	require.Equal(t, PublishExpirySweep, next)
}
