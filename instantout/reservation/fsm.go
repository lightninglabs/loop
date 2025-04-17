package reservation

import (
	"context"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
)

type ProtocolVersion uint32

const (
	// ProtocolVersionServerInitiated is the protocol version where the
	// server initiates the reservation.
	ProtocolVersionServerInitiated ProtocolVersion = 1

	// ProtocolVersionClientInitiated is the protocol version where the
	// client initiates the reservation.
	ProtocolVersionClientInitiated ProtocolVersion = 2
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
	ReservationClient swapserverrpc.ReservationServiceClient

	// LightningClient is the lnd client used to handle invoices decoding.
	LightningClient lndclient.LightningClient

	// RouterClient is used to send the offchain payments.
	RouterClient lndclient.RouterClient

	// NotificationManager is the manager that handles the notification
	// subscriptions.
	NotificationManager NotificationManager
}

// FSM is the state machine that manages the reservation lifecycle.
type FSM struct {
	*fsm.StateMachine

	cfg *Config

	reservation *Reservation
}

// NewFSM creates a new reservation FSM.
func NewFSM(cfg *Config, protocolVersion ProtocolVersion) *FSM {
	reservation := &Reservation{
		State:           fsm.EmptyState,
		ProtocolVersion: protocolVersion,
	}

	return NewFSMFromReservation(cfg, reservation)
}

// NewFSMFromReservation creates a new reservation FSM from an existing
// reservation recovered from the database.
func NewFSMFromReservation(cfg *Config, reservation *Reservation) *FSM {
	reservationFsm := &FSM{
		cfg:         cfg,
		reservation: reservation,
	}

	var states fsm.States
	switch reservation.ProtocolVersion {
	case ProtocolVersionServerInitiated:
		states = reservationFsm.GetServerInitiatedReservationStates()

	case ProtocolVersionClientInitiated:
		states = reservationFsm.GetClientInitiatedReservationStates()

	default:
		states = make(fsm.States)
	}

	reservationFsm.StateMachine = fsm.NewStateMachineWithState(
		states, reservation.State, defaultObserverSize,
	)

	reservationFsm.ActionEntryFunc = reservationFsm.updateReservation

	return reservationFsm
}

// States.
var (
	// Init is the initial state of the reservation.
	Init = fsm.StateType("Init")

	// SendPrepaymentPayment is the state where the client sends the payment to the
	// server.
	SendPrepaymentPayment = fsm.StateType("SendPayment")

	// WaitForConfirmation is the state where we wait for the reservation
	// tx to be confirmed.
	WaitForConfirmation = fsm.StateType("WaitForConfirmation")

	// Confirmed is the state where the reservation tx has been confirmed.
	Confirmed = fsm.StateType("Confirmed")

	// TimedOut is the state where the reservation has timed out.
	TimedOut = fsm.StateType("TimedOut")

	// Failed is the state where the reservation has failed.
	Failed = fsm.StateType("Failed")

	// Spent is the state where a spend tx has been confirmed.
	Spent = fsm.StateType("Spent")

	// Locked is the state where the reservation is locked and can't be
	// used for instant out swaps.
	Locked = fsm.StateType("Locked")
)

// Events.
var (
	// OnServerRequest is the event that is triggered when the server
	// requests a new reservation.
	OnServerRequest = fsm.EventType("OnServerRequest")

	// OnClientInitialized is the event that is triggered when the client
	// has initialized the reservation.
	OnClientInitialized = fsm.EventType("OnClientInitialized")

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

	// OnSpent is the event that is triggered when the reservation has been
	// spent.
	OnSpent = fsm.EventType("OnSpent")

	// OnLocked is the event that is triggered when the reservation has
	// been locked.
	OnLocked = fsm.EventType("OnLocked")

	// OnUnlocked is the event that is triggered when the reservation has
	// been unlocked.
	OnUnlocked = fsm.EventType("OnUnlocked")
)

// GetClientInitiatedReservationStates returns the statemap that defines the
// reservation state machine, where the client initiates the reservation.
func (f *FSM) GetClientInitiatedReservationStates() fsm.States {
	return fsm.States{
		fsm.EmptyState: fsm.State{
			Transitions: fsm.Transitions{
				OnClientInitialized: Init,
			},
			Action: nil,
		},
		Init: fsm.State{
			Transitions: fsm.Transitions{
				OnClientInitialized: SendPrepaymentPayment,
				OnRecover:           Failed,
				fsm.OnError:         Failed,
			},
			Action: f.InitFromClientRequestAction,
		},
		SendPrepaymentPayment: fsm.State{
			Transitions: fsm.Transitions{
				OnBroadcast: WaitForConfirmation,
				OnRecover:   SendPrepaymentPayment,
				fsm.OnError: Failed,
			},
			Action: f.SendPrepayment,
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
				OnSpent:     Spent,
				OnTimedOut:  TimedOut,
				OnRecover:   Confirmed,
				OnLocked:    Locked,
				fsm.OnError: Confirmed,
			},
			Action: f.AsyncWaitForExpiredOrSweptAction,
		},
		Locked: fsm.State{
			Transitions: fsm.Transitions{
				OnUnlocked:  Confirmed,
				OnTimedOut:  TimedOut,
				OnRecover:   Locked,
				OnSpent:     Spent,
				fsm.OnError: Locked,
			},
			Action: f.AsyncWaitForExpiredOrSweptAction,
		},
		TimedOut: fsm.State{
			Transitions: fsm.Transitions{
				OnTimedOut: TimedOut,
			},
			Action: fsm.NoOpAction,
		},

		Spent: fsm.State{
			Transitions: fsm.Transitions{
				OnSpent: Spent,
			},
			Action: fsm.NoOpAction,
		},

		Failed: fsm.State{
			Action: fsm.NoOpAction,
		},
	}
}

// GetServerInitiatedReservationStates returns the statemap that defines the
// reservation state machine, where the server initiates the reservation.
func (f *FSM) GetServerInitiatedReservationStates() fsm.States {
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
			Action: f.InitFromServerRequestAction,
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
				OnSpent:     Spent,
				OnTimedOut:  TimedOut,
				OnRecover:   Confirmed,
				OnLocked:    Locked,
				fsm.OnError: Confirmed,
			},
			Action: f.AsyncWaitForExpiredOrSweptAction,
		},
		Locked: fsm.State{
			Transitions: fsm.Transitions{
				OnUnlocked:  Confirmed,
				OnTimedOut:  TimedOut,
				OnRecover:   Locked,
				OnSpent:     Spent,
				fsm.OnError: Locked,
			},
			Action: f.AsyncWaitForExpiredOrSweptAction,
		},
		TimedOut: fsm.State{
			Transitions: fsm.Transitions{
				OnTimedOut: TimedOut,
			},
			Action: fsm.NoOpAction,
		},

		Spent: fsm.State{
			Transitions: fsm.Transitions{
				OnSpent: Spent,
			},
			Action: fsm.NoOpAction,
		},

		Failed: fsm.State{
			Action: fsm.NoOpAction,
		},
	}
}

// updateReservation updates the reservation in the database. This function
// is called after every new state transition.
func (r *FSM) updateReservation(ctx context.Context,
	notification fsm.Notification) {

	if r.reservation == nil {
		return
	}

	r.Debugf(
		"NextState: %v, PreviousState: %v, Event: %v",
		notification.NextState, notification.PreviousState,
		notification.Event,
	)

	r.reservation.State = notification.NextState

	// Don't update the reservation if we are in an initial state or if we
	// are transitioning from an initial state to a failed state.
	if r.reservation.State == fsm.EmptyState ||
		r.reservation.State == Init ||
		(notification.PreviousState == Init &&
			r.reservation.State == Failed) {

		return
	}

	err := r.cfg.Store.UpdateReservation(ctx, r.reservation)
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
	case Failed, TimedOut, Spent:
		return true
	}
	return false
}
