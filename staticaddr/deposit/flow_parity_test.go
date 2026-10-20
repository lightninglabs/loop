package deposit

import (
	"testing"

	"github.com/lightninglabs/loop/fsm"
	"github.com/stretchr/testify/require"
)

// TestBitcoinDepositTransitionParity preserves every pre-extraction transition.
func TestBitcoinDepositTransitionParity(t *testing.T) {
	expected := map[fsm.StateType]fsm.Transitions{
		fsm.EmptyState: {
			OnStart: Deposited,
		},
		Deposited: {
			OnExpiry:              PublishExpirySweep,
			OnWithdrawInitiated:   Withdrawing,
			OnLoopInInitiated:     LoopingIn,
			OnOpeningChannel:      OpeningChannel,
			OnSweepingHtlcTimeout: SweepHtlcTimeout,
			OnRecover:             Deposited,
			fsm.OnError:           Deposited,
		},
		PublishExpirySweep: {
			OnRecover:         PublishExpirySweep,
			OnExpiryPublished: WaitForExpirySweep,
			fsm.OnError:       Deposited,
		},
		WaitForExpirySweep: {
			OnExpirySwept: Expired,
			OnRecover:     PublishExpirySweep,
			fsm.OnError:   Deposited,
		},
		Expired: {
			OnExpiry: Expired,
		},
		Withdrawing: {
			OnWithdrawn:         Withdrawn,
			OnWithdrawInitiated: Withdrawing,
			OnRecover:           Withdrawing,
			OnExpiry:            Withdrawing,
			fsm.OnError:         Deposited,
		},
		LoopingIn: {
			OnLoopedIn:            LoopedIn,
			OnExpiry:              PublishExpirySweep,
			OnSweepingHtlcTimeout: SweepHtlcTimeout,
			OnLoopInInitiated:     LoopingIn,
			OnRecover:             LoopingIn,
			fsm.OnError:           Deposited,
		},
		LoopedIn: {
			OnExpiry: LoopedIn,
		},
		SweepHtlcTimeout: {
			OnHtlcTimeoutSwept: HtlcTimeoutSwept,
			OnRecover:          SweepHtlcTimeout,
		},
		HtlcTimeoutSwept: {
			OnExpiry: HtlcTimeoutSwept,
		},
		Withdrawn: {
			OnExpiry:    Withdrawn,
			OnWithdrawn: Withdrawn,
		},
		OpeningChannel: {
			fsm.OnError:        Deposited,
			OnChannelPublished: ChannelPublished,
			OnRecover:          OpeningChannel,
			OnExpiry:           OpeningChannel,
		},
		ChannelPublished: {
			OnExpiry: ChannelPublished,
		},
	}
	actual := (&FSM{}).DepositStatesV0()
	require.Len(t, actual, len(expected))
	for state, transitions := range expected {
		require.Equal(t, transitions, actual[state].Transitions, state)
		require.NotNil(t, actual[state].Action, state)
	}
}
