package fsm

import "sync"

type GenericFSM[T any] struct {
	*StateMachine

	Val     *T
	ValLock sync.RWMutex
}

// NewGenericFSM creates a new generic FSM with the given initial state and
// value.
func NewGenericFSM[T any](fsm *StateMachine, val *T) *GenericFSM[T] {
	return &GenericFSM[T]{
		StateMachine: fsm,
		Val:          val,
	}
}

type callOptions struct {
	withMainMutex bool
}

// CallOption is a functional option that can be used to modify the runFuncs.
type CallOption func(*callOptions)

// WithMainMutex is an option that can be used to run the function with the main
// mutex locked. This requires the FSM to not be currently running an action.
func WithMainMutex() CallOption {
	return func(o *callOptions) {
		o.withMainMutex = true
	}
}

// RunFunc runs the given function in the FSM. It locks the FSM value lock
// before running the function and unlocks it after the function is done.
func (fsm *GenericFSM[T]) RunFunc(fn func(val *T) error, options ...CallOption,
) error {

	opts := &callOptions{}
	for _, option := range options {
		option(opts)
	}

	fsm.ValLock.Lock()
	defer fsm.ValLock.Unlock()
	if opts.withMainMutex {
		fsm.mutex.Lock()
		defer fsm.mutex.Unlock()
	}

	return fn(fsm.Val)
}

// GetVal returns the value of the FSM.
func (fsm *GenericFSM[T]) GetVal(options ...CallOption) *T {
	opts := &callOptions{}
	for _, option := range options {
		option(opts)
	}

	fsm.ValLock.RLock()
	defer fsm.ValLock.RUnlock()

	if opts.withMainMutex {
		fsm.mutex.Lock()
		defer fsm.mutex.Unlock()
	}

	return fsm.Val
}
