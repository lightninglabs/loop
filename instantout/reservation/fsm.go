package reservation

import (
	"context"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	looprpc "github.com/lightninglabs/loop/swapserverrpc"
)

const (
	// defaultObserverSize is the size of the fsm observer channel.
	defaultObserverSize = 15
)

// Config contains all the services that the reservation FSM needs to operate.
type Config struct {
	// Store is the database store for the reservations.
	Store Store

	// Wallet handles the key derivation for the reservation.
	Wallet lndclient.WalletKitClient

	// ChainNotifier is used to subscribe to block notifications.
	ChainNotifier lndclient.ChainNotifierClient

	// ReservationClient is the client used to communicate with the
	// swap server.
	ReservationClient looprpc.ReservationServiceClient

	// FetchL402 is the function used to fetch the l402 token.
	FetchL402 func(context.Context) error
}

// FSM is the state machine that manages the reservation lifecycle.
type FSM struct {
	*fsm.StateMachine

	cfg *Config

	reservation *Reservation

	ctx context.Context
}

// NewFSM creates a new reservation FSM.
func NewFSM(ctx context.Context, cfg *Config) *FSM {
	reservation := &Reservation{
		State: fsm.EmptyState,
	}

	return NewFSMFromReservation(ctx, cfg, reservation)
}

// NewFSMFromReservation creates a new reservation FSM from an existing
// reservation recovered from the database.
func NewFSMFromReservation(ctx context.Context, cfg *Config,
	reservation *Reservation) *FSM {

	reservationFsm := &FSM{
		ctx:         ctx,
		cfg:         cfg,
		reservation: reservation,
	}

	reservationFsm.StateMachine = fsm.NewStateMachineWithState(
		reservationFsm.GetReservationStates(), reservation.State,
		defaultObserverSize,
	)
	reservationFsm.ActionEntryFunc = reservationFsm.updateReservation

	return reservationFsm
}

// States.
var (
	// Init is the initial state of the reservation.
	Init = fsm.StateType("Init")

	// WaitForConfirmation is the state where we wait for the reservation
	// tx to be confirmed.
	WaitForConfirmation = fsm.StateType("WaitForConfirmation")

	// Confirmed is the state where the reservation tx has been confirmed.
	Confirmed = fsm.StateType("Confirmed")

	// TimedOut is the state where the reservation has timed out.
	TimedOut = fsm.StateType("TimedOut")

	// Failed is the state where the reservation has failed.
	Failed = fsm.StateType("Failed")

	// Swept is the state where the reservation has been swept by the server.
	Swept = fsm.StateType("Swept")
)

// Events.
var (
	// OnServerRequest is the event that is triggered when the server
	// requests a new reservation.
	OnServerRequest = fsm.EventType("OnServerRequest")

	// OnBroadcast is the event that is triggered when the reservation tx
	// has been broadcast.
	OnBroadcast = fsm.EventType("OnBroadcast")

	// OnConfirmed is the event that is triggered when the reservation tx
	// has been confirmed.
	OnConfirmed = fsm.EventType("OnConfirmed")

	// OnTimedOut is the event that is triggered when the reservation has
	// timed out.
	OnTimedOut = fsm.EventType("OnTimedOut")

	// OnSwept is the event that is triggered when the reservation has been
	// swept by the server.
	OnSwept = fsm.EventType("OnSwept")

	// OnRecover is the event that is triggered when the reservation FSM
	// recovers from a restart.
	OnRecover = fsm.EventType("OnRecover")
)

// GetReservationStates returns the statemap that defines the reservation
// state machine.
func (f *FSM) GetReservationStates() fsm.States {
	return fsm.States{
		fsm.EmptyState: fsm.State{
			Transitions: fsm.Transitions{
				OnServerRequest: Init,
			},
			Action: nil,
		},
		Init: fsm.State{
			Transitions: fsm.Transitions{
				OnBroadcast: WaitForConfirmation,
				OnRecover:   Failed,
				fsm.OnError: Failed,
			},
			Action: f.InitAction,
		},
		WaitForConfirmation: fsm.State{
			Transitions: fsm.Transitions{
				OnRecover:   WaitForConfirmation,
				OnConfirmed: Confirmed,
				OnTimedOut:  TimedOut,
			},
			Action: f.SubscribeToConfirmationAction,
		},
		Confirmed: fsm.State{
			Transitions: fsm.Transitions{
				OnTimedOut: TimedOut,
				OnRecover:  Confirmed,
			},
			Action: f.ReservationConfirmedAction,
		},
		TimedOut: fsm.State{
			Action: fsm.NoOpAction,
		},
		Failed: fsm.State{
			Action: fsm.NoOpAction,
		},
	}
}

// updateReservation updates the reservation in the database. This function
// is called after every new state transition.
func (r *FSM) updateReservation(notification fsm.Notification) {
	if r.reservation == nil {
		return
	}

	r.Debugf(
		"NextState: %v, PreviousState: %v, Event: %v",
		notification.NextState, notification.PreviousState,
		notification.Event,
	)

	r.reservation.State = notification.NextState

	err := r.cfg.Store.UpdateReservation(r.ctx, r.reservation)
	if err != nil {
		r.Errorf("unable to update reservation: %v", err)
	}
}

func (r *FSM) Infof(format string, args ...interface{}) {
	log.Infof(
		"Reservation %x: "+format,
		append([]interface{}{r.reservation.ID}, args...)...,
	)
}

func (r *FSM) Debugf(format string, args ...interface{}) {
	log.Debugf(
		"Reservation %x: "+format,
		append([]interface{}{r.reservation.ID}, args...)...,
	)
}

func (r *FSM) Errorf(format string, args ...interface{}) {
	log.Errorf(
		"Reservation %x: "+format,
		append([]interface{}{r.reservation.ID}, args...)...,
	)
}

// isFinalState returns true if the state is a final state.
func isFinalState(state fsm.StateType) bool {
	switch state {
	case Failed, Swept, TimedOut:
		return true
	}
	return false
}
