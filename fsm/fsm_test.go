package fsm

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	errAction = errors.New("action error")
)

// TestStateMachineContext is a test context for the state machine.
type TestStateMachineContext struct {
	*StateMachine
}

// GetStates returns the states for the test state machine.
// The StateMap looks like this:
// State1 -> Event1 -> State2 .
func (c *TestStateMachineContext) GetStates() States {
	return States{
		"State1": State{
			Action: func(ctx EventContext) EventType {
				return "Event1"
			},
			Transitions: Transitions{
				"Event1": "State2",
			},
		},
		"State2": State{
			Action: func(ctx EventContext) EventType {
				return "NoOp"
			},
			Transitions: Transitions{},
		},
	}
}

// errorAction returns an error.
func (c *TestStateMachineContext) errorAction(eventCtx EventContext) EventType {
	return c.StateMachine.HandleError(errAction)
}

func setupTestStateMachineContext() *TestStateMachineContext {
	ctx := &TestStateMachineContext{}

	ctx.StateMachine = &StateMachine{
		States:   ctx.GetStates(),
		current:  "State1",
		previous: "",
	}

	return ctx
}

// TestStateMachine_Success tests the state machine with a successful event.
func TestStateMachine_Success(t *testing.T) {
	ctx := setupTestStateMachineContext()

	// Send an event to the state machine.
	err := ctx.SendEvent("Event1", nil)
	require.NoError(t, err)

	// Check that the state machine has transitioned to the next state.
	require.Equal(t, StateType("State2"), ctx.current)
}

// TestStateMachine_ConfigurationError tests the state machine with a
// configuration error.
func TestStateMachine_ConfigurationError(t *testing.T) {
	ctx := setupTestStateMachineContext()
	ctx.StateMachine.States = nil

	err := ctx.SendEvent("Event1", nil)
	require.EqualError(
		t, err,
		NewErrConfigError("state machine config is nil").Error(),
	)
}

// TestStateMachine_ActionError tests the state machine with an action error.
func TestStateMachine_ActionError(t *testing.T) {
	ctx := setupTestStateMachineContext()

	states := ctx.StateMachine.States

	// Add a Transition to State2 if the Action on Stat2 fails.
	// The new StateMap looks like this:
	// 	State1 -> Event1 -> State2
	//
	// 	State2 -> OnError -> ErrorState
	states["State2"] = State{
		Action: ctx.errorAction,
		Transitions: Transitions{
			OnError: "ErrorState",
		},
	}

	states["ErrorState"] = State{
		Action: func(ctx EventContext) EventType {
			return "NoOp"
		},
		Transitions: Transitions{},
	}

	err := ctx.SendEvent("Event1", nil)

	// Sending an event to the state machine should not return an error.
	require.NoError(t, err)

	// Ensure that the last error is set.
	require.Equal(t, errAction, ctx.StateMachine.LastActionError)

	// Expect the state machine to have transitioned to the ErrorState.
	require.Equal(t, StateType("ErrorState"), ctx.StateMachine.current)
}
