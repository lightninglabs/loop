package fsm

import (
	"fmt"
)

// ExampleService is an example service that we want to wait for in the FSM.
type ExampleService interface {
	WaitForStuffHappening() (<-chan bool, error)
}

// ExampleStore is an example store that we want to use in our exitFunc.
type ExampleStore interface {
	StoreStuff() error
}

// ExampleFSM implements the FSM and uses the ExampleService and ExampleStore
// to implement the actions.
type ExampleFSM struct {
	*StateMachine

	service ExampleService
	store   ExampleStore
}

// NewExampleFSMContext creates a new example FSM context.
func NewExampleFSMContext(service ExampleService,
	store ExampleStore) *ExampleFSM {

	exampleFSM := &ExampleFSM{
		service: service,
		store:   store,
	}
	exampleFSM.StateMachine = NewStateMachine(exampleFSM.GetStates())

	return exampleFSM
}

// States.
const (
	InitFSM         = StateType("InitFSM")
	StuffSentOut    = StateType("StuffSentOut")
	WaitingForStuff = StateType("WaitingForStuff")
	StuffFailed     = StateType("StuffFailed")
	StuffSuccess    = StateType("StuffSuccess")
)

// Events.
var (
	OnRequestStuff = EventType("OnRequestStuff")
	OnStuffSentOut = EventType("OnStuffSentOut")
	OnStuffSuccess = EventType("OnStuffSuccess")
)

// GetStates returns the states for the example FSM.
func (e *ExampleFSM) GetStates() States {
	return States{
		Default: State{
			Transitions: Transitions{
				OnRequestStuff: InitFSM,
			},
		},
		InitFSM: State{
			Action: e.initFSM,
			Transitions: Transitions{
				OnStuffSentOut: StuffSentOut,
				OnError:        StuffFailed,
			},
		},
		StuffSentOut: State{
			Action: e.waitForStuff,
			Transitions: Transitions{
				OnStuffSuccess: StuffSuccess,
				OnError:        StuffFailed,
			},
		},
		StuffFailed: State{
			Action: NoOpAction,
		},
		StuffSuccess: State{
			Action: NoOpAction,
		},
	}
}

// InitStuffRequest is the event context for the InitFSM state.
type InitStuffRequest struct {
	Stuff       string
	respondChan chan<- string
}

// initFSM is the action for the InitFSM state.
func (e *ExampleFSM) initFSM(eventCtx EventContext) EventType {
	req, ok := eventCtx.(*InitStuffRequest)
	if !ok {
		return e.HandleError(
			fmt.Errorf("invalid event context type: %T", eventCtx),
		)
	}

	err := e.store.StoreStuff()
	if err != nil {
		return e.HandleError(err)
	}

	req.respondChan <- req.Stuff

	return OnStuffSentOut
}

// waitForStuff is an action that waits for stuff to happen.
func (e *ExampleFSM) waitForStuff(eventCtx EventContext) EventType {
	waitChan, err := e.service.WaitForStuffHappening()
	if err != nil {
		return e.HandleError(err)
	}

	go func() {
		<-waitChan
		err := e.SendEvent(OnStuffSuccess, nil)
		if err != nil {
			log.Errorf("unable to send event: %v", err)
		}
	}()

	return NoOp
}
