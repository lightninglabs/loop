package instantout

import (
	"context"
	"errors"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
)

type ProtocolVersion uint32

const (
	// ProtocolVersionUndefined is the undefined protocol version.
	ProtocolVersionUndefined ProtocolVersion = 0

	// ProtocolVersionFullReservation is the protocol version that uses
	// the full reservation amount without change.
	ProtocolVersionFullReservation ProtocolVersion = 1
)

// CurrentProtocolVersion returns the current protocol version.
func CurrentProtocolVersion() ProtocolVersion {
	return ProtocolVersionFullReservation
}

// CurrentRpcProtocolVersion returns the current rpc protocol version.
func CurrentRpcProtocolVersion() swapserverrpc.InstantOutProtocolVersion {
	return swapserverrpc.InstantOutProtocolVersion(CurrentProtocolVersion())
}

const (
	// defaultObserverSize is the size of the fsm observer channel.
	defaultObserverSize = 15
)

var (
	ErrProtocolVersionNotSupported = errors.New(
		"protocol version not supported",
	)
)

// States.
var (
	// Init is the initial state of the instant out FSM.
	Init = fsm.StateType("Init")

	// SendPaymentAndPollAccepted is the state where the payment is sent
	// and the server is polled for the accepted state.
	SendPaymentAndPollAccepted = fsm.StateType("SendPaymentAndPollAccepted")

	// BuildHtlc is the state where the htlc transaction is built.
	BuildHtlc = fsm.StateType("BuildHtlc")

	// PushPreimage is the state where the preimage is pushed to the server.
	PushPreimage = fsm.StateType("PushPreimage")

	// WaitForSweeplessSweepConfirmed is the state where we wait for the
	// sweepless sweep to be confirmed.
	WaitForSweeplessSweepConfirmed = fsm.StateType(
		"WaitForSweeplessSweepConfirmed")

	// FinishedSweeplessSweep is the state where the swap is finished by
	// publishing the sweepless sweep.
	FinishedSweeplessSweep = fsm.StateType("FinishedSweeplessSweep")

	// PublishHtlc is the state where the htlc transaction is published.
	PublishHtlc = fsm.StateType("PublishHtlc")

	// PublishHtlcSweep is the state where the htlc sweep transaction is
	// published.
	PublishHtlcSweep = fsm.StateType("PublishHtlcSweep")

	// FinishedHtlcPreimageSweep is the state where the swap is finished by
	// publishing the htlc preimage sweep.
	FinishedHtlcPreimageSweep = fsm.StateType("FinishedHtlcPreimageSweep")

	// WaitForHtlcSweepConfirmed is the state where we wait for the htlc
	// sweep to be confirmed.
	WaitForHtlcSweepConfirmed = fsm.StateType("WaitForHtlcSweepConfirmed")

	// FailedHtlcSweep is the state where the htlc sweep failed.
	FailedHtlcSweep = fsm.StateType("FailedHtlcSweep")

	// Failed is the state where the swap failed.
	Failed = fsm.StateType("InstantOutFailed")
)

// Events.
var (
	// OnStart is the event that is sent when the FSM is started.
	OnStart = fsm.EventType("OnStart")

	// OnInit is the event that is triggered when the FSM is initialized.
	OnInit = fsm.EventType("OnInit")

	// OnPaymentAccepted is the event that is triggered when the payment
	// is accepted by the server.
	OnPaymentAccepted = fsm.EventType("OnPaymentAccepted")

	// OnHtlcSigReceived is the event that is triggered when the htlc sig
	// is received.
	OnHtlcSigReceived = fsm.EventType("OnHtlcSigReceived")

	// OnPreimagePushed is the event that is triggered when the preimage
	// is pushed to the server.
	OnPreimagePushed = fsm.EventType("OnPreimagePushed")

	// OnSweeplessSweepPublished is the event that is triggered when the
	// sweepless sweep is published.
	OnSweeplessSweepPublished = fsm.EventType("OnSweeplessSweepPublished")

	// OnSweeplessSweepConfirmed is the event that is triggered when the
	// sweepless sweep is confirmed.
	OnSweeplessSweepConfirmed = fsm.EventType("OnSweeplessSweepConfirmed")

	// OnErrorPublishHtlc is the event that is triggered when the htlc
	// sweep is published after an error.
	OnErrorPublishHtlc = fsm.EventType("OnErrorPublishHtlc")

	// OnInvalidCoopSweep is the event that is triggered when the coop
	// sweep is invalid.
	OnInvalidCoopSweep = fsm.EventType("OnInvalidCoopSweep")

	// OnHtlcPublished is the event that is triggered when the htlc
	// transaction is published.
	OnHtlcPublished = fsm.EventType("OnHtlcPublished")

	// OnHtlcSweepPublished is the event that is triggered when the htlc
	// sweep is published.
	OnHtlcSweepPublished = fsm.EventType("OnHtlcSweepPublished")

	// OnHtlcSwept is the event that is triggered when the htlc sweep is
	// confirmed.
	OnHtlcSwept = fsm.EventType("OnHtlcSwept")

	// OnRecover is the event that is triggered when the FSM recovers from
	// a restart.
	OnRecover = fsm.EventType("OnRecover")
)

// Config contains the services required for the instant out FSM.
type Config struct {
	// Store is used to store the instant out.
	Store InstantLoopOutStore

	// LndClient is used to decode the swap invoice.
	LndClient lndclient.LightningClient

	// RouterClient is used to send the offchain payment to the server.
	RouterClient lndclient.RouterClient

	// ChainNotifier is used to be notified of on-chain events.
	ChainNotifier lndclient.ChainNotifierClient

	// Signer is used to sign transactions.
	Signer lndclient.SignerClient

	// Wallet is used to derive keys.
	Wallet lndclient.WalletKitClient

	// InstantOutClient is used to communicate with the swap server.
	InstantOutClient swapserverrpc.InstantSwapServerClient

	// ReservationManager is used to get the reservations and lock them.
	ReservationManager ReservationManager

	// Network is the network that is used for the swap.
	Network *chaincfg.Params
}

// FSM is the state machine that handles the instant out.
type FSM struct {
	*fsm.StateMachine

	// cfg contains all the services that the reservation manager needs to
	// operate.
	cfg *Config

	// InstantOut contains all the information about the instant out.
	InstantOut *InstantOut

	// htlcMusig2Sessions contains all the reservations input musig2
	// sessions that will be used for the htlc transaction.
	htlcMusig2Sessions []*input.MuSig2SessionInfo

	// sweeplessSweepSessions contains all the reservations input musig2
	// sessions that will be used for the sweepless sweep transaction.
	sweeplessSweepSessions []*input.MuSig2SessionInfo
}

// NewFSM creates a new instant out FSM.
func NewFSM(cfg *Config, protocolVersion ProtocolVersion) (*FSM, error) {
	instantOut := &InstantOut{
		State:           fsm.EmptyState,
		protocolVersion: protocolVersion,
	}

	return NewFSMFromInstantOut(cfg, instantOut)
}

// NewFSMFromInstantOut creates a new instantout FSM from an existing instantout
// recovered from the database.
func NewFSMFromInstantOut(cfg *Config, instantOut *InstantOut) (*FSM, error) {
	instantOutFSM := &FSM{
		cfg:        cfg,
		InstantOut: instantOut,
	}
	switch instantOut.protocolVersion {
	case ProtocolVersionFullReservation:
		instantOutFSM.StateMachine = fsm.NewStateMachineWithState(
			instantOutFSM.GetV1ReservationStates(),
			instantOut.State, defaultObserverSize,
		)

	default:
		return nil, ErrProtocolVersionNotSupported
	}

	instantOutFSM.ActionEntryFunc = instantOutFSM.updateInstantOut

	return instantOutFSM, nil
}

// GetV1ReservationStates returns the states for the v1 reservation.
func (f *FSM) GetV1ReservationStates() fsm.States {
	return fsm.States{
		fsm.EmptyState: fsm.State{
			Transitions: fsm.Transitions{
				OnStart: Init,
			},
			Action: nil,
		},
		Init: fsm.State{
			Transitions: fsm.Transitions{
				OnInit:      SendPaymentAndPollAccepted,
				fsm.OnError: Failed,
				OnRecover:   Failed,
			},
			Action: f.InitInstantOutAction,
		},
		SendPaymentAndPollAccepted: fsm.State{
			Transitions: fsm.Transitions{
				OnPaymentAccepted: BuildHtlc,
				fsm.OnError:       Failed,
				OnRecover:         Failed,
			},
			Action: f.PollPaymentAcceptedAction,
		},
		BuildHtlc: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcSigReceived: PushPreimage,
				fsm.OnError:       Failed,
				OnRecover:         Failed,
			},
			Action: f.BuildHTLCAction,
		},
		PushPreimage: fsm.State{
			Transitions: fsm.Transitions{
				OnSweeplessSweepPublished: WaitForSweeplessSweepConfirmed,
				fsm.OnError:               Failed,
				OnErrorPublishHtlc:        PublishHtlc,
				OnRecover:                 PushPreimage,
			},
			Action: f.PushPreimageAction,
		},
		WaitForSweeplessSweepConfirmed: fsm.State{
			Transitions: fsm.Transitions{
				OnSweeplessSweepConfirmed: FinishedSweeplessSweep,
				OnRecover:                 WaitForSweeplessSweepConfirmed,
				fsm.OnError:               PublishHtlc,
			},
			Action: f.WaitForSweeplessSweepConfirmedAction,
		},
		FinishedSweeplessSweep: fsm.State{
			Transitions: fsm.Transitions{},
			Action:      fsm.NoOpAction,
		},
		PublishHtlc: fsm.State{
			Transitions: fsm.Transitions{
				fsm.OnError:     FailedHtlcSweep,
				OnRecover:       PublishHtlc,
				OnHtlcPublished: PublishHtlcSweep,
			},
			Action: f.PublishHtlcAction,
		},
		PublishHtlcSweep: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcSweepPublished: WaitForHtlcSweepConfirmed,
				OnRecover:            PublishHtlcSweep,
				fsm.OnError:          FailedHtlcSweep,
			},
			Action: f.PublishHtlcSweepAction,
		},
		WaitForHtlcSweepConfirmed: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcSwept: FinishedHtlcPreimageSweep,
				OnRecover:   WaitForHtlcSweepConfirmed,
				fsm.OnError: FailedHtlcSweep,
			},
			Action: f.WaitForHtlcSweepConfirmedAction,
		},
		FinishedHtlcPreimageSweep: fsm.State{
			Transitions: fsm.Transitions{},
			Action:      fsm.NoOpAction,
		},
		FailedHtlcSweep: fsm.State{
			Action: fsm.NoOpAction,
			Transitions: fsm.Transitions{
				OnRecover: PublishHtlcSweep,
			},
		},
		Failed: fsm.State{
			Action: fsm.NoOpAction,
		},
	}
}

// updateInstantOut is called after every action and updates the reservation
// in the db.
func (f *FSM) updateInstantOut(ctx context.Context,
	notification fsm.Notification) {

	f.Infof("Previous: %v, Event: %v, Next: %v", notification.PreviousState,
		notification.Event, notification.NextState)

	// Skip the update if the reservation is not yet initialized.
	if f.InstantOut == nil {
		return
	}

	f.InstantOut.State = notification.NextState

	// If we're in the early stages we don't have created the reservation
	// in the store yet and won't need to update it.
	if f.InstantOut.State == Init ||
		f.InstantOut.State == fsm.EmptyState ||
		(notification.PreviousState == Init &&
			f.InstantOut.State == Failed) {

		return
	}

	err := f.cfg.Store.UpdateInstantLoopOut(ctx, f.InstantOut)
	if err != nil {
		log.Errorf("Error updating instant out: %v", err)
		return
	}
}

// Infof logs an info message with the reservation hash as prefix.
func (f *FSM) Infof(format string, args ...interface{}) {
	log.Infof(
		"InstantOut %v: "+format,
		append(
			[]interface{}{f.InstantOut.swapPreimage.Hash()},
			args...,
		)...,
	)
}

// Debugf logs a debug message with the reservation hash as prefix.
func (f *FSM) Debugf(format string, args ...interface{}) {
	log.Debugf(
		"InstantOut %v: "+format,
		append(
			[]interface{}{f.InstantOut.swapPreimage.Hash()},
			args...,
		)...,
	)
}

// Errorf logs an error message with the reservation hash as prefix.
func (f *FSM) Errorf(format string, args ...interface{}) {
	log.Errorf(
		"InstantOut %v: "+format,
		append(
			[]interface{}{f.InstantOut.swapPreimage.Hash()},
			args...,
		)...,
	)
}

// isFinalState returns true if the state is a final state.
func isFinalState(state fsm.StateType) bool {
	switch state {
	case Failed, FinishedHtlcPreimageSweep,
		FinishedSweeplessSweep:

		return true
	}
	return false
}
