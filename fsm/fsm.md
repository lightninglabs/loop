# Finite State Machine Module

This module provides a simple golang finite state machine (FSM) implementation.


## Introduction

The state machine uses events and actions to transition between states. The
events are used to trigger a transition and the actions are used to perform
some work when entering a state. Actions return new events which are then
used to trigger the next transition.

## Usage

A simple way to use the FSM is to embed it into a struct:

```go
type LightSwitchFSM struct {
	*StateMachine
}
```

In order to use the FSM you need to define the events, actions and statemaps
for the FSM. events are defined as constants, actions are defined as functions
on the `LightSwitchFSM` struct and statemaps are in a map of `State` to `StateMap`
where `StateMap` is a map of `Event` to `Action`.

For the `LightSwitchFSM` we can first define the states
```go
const (
	OffState = StateType("Off")
	OnState  = StateType("On")
)

const (
	SwitchOff = EventType("SwitchOff")
	SwitchOn  = EventType("SwitchOn")
)
```

Next we define the actions, here we're simply going to log from the action.
```go
func (a *LightSwitchFSM) OffAction(_ EventContext) EventType {
	fmt.Println("The light has been switched off")
	return NoOp
}

func (a *LightSwitchFSM) OnAction(_ EventContext) EventType {
	fmt.Println("The light has been switched on")
	return NoOp
}
```

Next we define the statemap, here we're going to implement a getStates() 
function that returns the statemap.
```go
func (l *LightSwitchFSM) getStates() States {
	return States{
		OffState: State{
			Action: l.OffAction,
			Transitions: Transitions{
				SwitchOn: OnState,
			},
		},
		OnState: State{
			Action: l.OnAction,
			Transitions: Transitions{
				SwitchOff: OffState,
			},
		},
	}
}
```

Finally, we can create the FSM and use it.

```go
func NewLightSwitchFSM() *LightSwitchFSM {
	fsm := &LightSwitchFSM{}
	fsm.StateMachine = &StateMachine{
		States:  fsm.getStates(),
		Current: OffState,
	}
	return fsm
}
```

This is what it would look like to use the FSM:
```go
func TestLightSwitchFSM(t *testing.T) {
	// Create a new light switch FSM.
	lightSwitch := NewLightSwitchFSM()

	// Expect the light to be off
	require.Equal(t, lightSwitch.Current, OffState)

	// Send the On Event
	err := lightSwitch.SendEvent(SwitchOn, nil)
	require.NoError(t, err)

	// Expect the light to be on
	require.Equal(t, lightSwitch.Current, OnState)

	// Send the Off Event
	err = lightSwitch.SendEvent(SwitchOff, nil)
	require.NoError(t, err)

	// Expect the light to be off
	require.Equal(t, lightSwitch.Current, OffState)
}
```

## Observing the state machine
The state machine can be observed by registering an observer. The observer
will be called when the state machine transitions between states. The observer
is called with the old state, the new state and the event that triggered the
transition.

An observer can be registered by calling the `RegisterObserver` function on
the state machine. The observer must implement the `Observer` interface.

```go
type Observer interface {
	Notify(Notification)
}
```

An example of a cached observer can be found in [observer.go](./observer.go).


## More Examples
A more elaborate example that uses error handling, event context and more 
elaborate actions can be found in here [examples_fsm.go](./example_fsm.go).
With the tests in [examples_fsm_test.go](./example_fsm_test.go) showing how to
use the FSM.

## Visualizing the FSM
The FSM can be visualized to mermaid markdown using the [stateparser.go](./stateparser/stateparser.go)
tool. The visualization for the exampleFSM can be found in [example_fsm.md](./example_fsm.md).