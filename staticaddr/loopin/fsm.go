package loopin

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightningnetwork/lnd/input"
)

// FSM embeds an FSM and extends it with a static address loop-in and a config.
type FSM struct {
	*fsm.StateMachine

	cfg *Config

	// loopIn stores the loop-in details that are relevant during the
	// lifetime of the swap.
	loopIn *StaticAddressLoopIn

	ctx context.Context

	// MuSig2 data must not be re-used across restarts, hence it is not
	// persisted.
	//
	// htlcMusig2Sessions contains all the loop-in musig2 sessions that will
	// be used for the htlc transaction.
	htlcMusig2Sessions []*input.MuSig2SessionInfo

	// htlcClientNonces contains all the nonces that the client sent for
	// the htlc musig2 sessions.
	htlcClientNonces [][musig2.PubNonceSize]byte

	// htlcServerNonces contains all the nonces that the server generated
	// for the htlc musig2 sessions.
	htlcServerNonces [][musig2.PubNonceSize]byte

	// htlcClientSigs contains all the signatures that the client generated
	// for the htlc musig2 sessions.
	htlcClientSigs [][]byte

	// sweeplessMusig2Sessions contains all the loop-in musig2 sessions that
	// will be used for signing the sweepless sweep transaction.
	sweeplessMusig2Sessions []*input.MuSig2SessionInfo

	// sweeplessClientNonces contains all the nonces that the client sent
	// for the sweepless musig2 sessions.
	sweeplessClientNonces [][musig2.PubNonceSize]byte

	// sweeplessServerNonces contains all the nonces that the server
	// generated for the sweepless musig2 sessions.
	sweeplessServerNonces [][musig2.PubNonceSize]byte

	// sweeplessClientSigs contains all the signatures that the client
	// generated for the sweepless sweep transaction.
	sweeplessClientSigs [][]byte
}

// NewFSM creates a new loop-in state machine.
func NewFSM(ctx context.Context, loopIn *StaticAddressLoopIn, cfg *Config,
	recoverStateMachine bool) (*FSM, error) {

	loopInFsm := &FSM{
		ctx:    ctx,
		cfg:    cfg,
		loopIn: loopIn,
	}

	params, err := cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to get static address "+
			"parameters: %v", err)
	}

	loopInStates := loopInFsm.LoopInStatesV0()
	switch params.ProtocolVersion {
	case version.ProtocolVersion_V0:

	default:
		return nil, deposit.ErrProtocolVersionNotSupported
	}

	if recoverStateMachine {
		loopInFsm.StateMachine = fsm.NewStateMachineWithState(
			loopInStates, loopIn.GetState(),
			deposit.DefaultObserverSize,
		)
	} else {
		loopInFsm.StateMachine = fsm.NewStateMachine(
			loopInStates, deposit.DefaultObserverSize,
		)
	}

	loopInFsm.ActionEntryFunc = loopInFsm.updateLoopIn

	return loopInFsm, nil
}

// States that the loop-in fsm can transition to.
var (
	// InitHtlc initiates the htlc creation with the server.
	InitHtlc = fsm.StateType("InitHtlc")

	// SignHtlcTx partially signs the htlc transaction with the received
	// server nonces. The client doesn't hold a final signature hence can't
	// publish the htlc.
	SignHtlcTx = fsm.StateType("SignHtlcTx")

	// MonitorInvoiceAndHtlcTx monitors the swap invoice payment and the
	// htlc transaction confirmation.
	// Since the client provided its partial signature to spend to the htlc
	// pkScript, the server could publish the htlc transaction prematurely.
	// We need to monitor the htlc transaction to sweep our timeout path in
	// this case.
	// If the server pays the swap invoice as expected we can stop to
	// monitor the htlc timeout path.
	MonitorInvoiceAndHtlcTx = fsm.StateType("MonitorInvoiceAndHtlcTx")

	// PaymentReceived is the state where the swap invoice was paid by the
	// server. The client can now sign the sweepless sweep transaction.
	PaymentReceived = fsm.StateType("PaymentReceived")

	// SweepHtlcTimout is the state where the htlc timeout path is published
	// because the server did not pay the invoice on time.
	SweepHtlcTimout = fsm.StateType("SweepHtlcTimout")

	// MonitorHtlcTimeoutSweep monitors the htlc timeout sweep transaction
	// confirmation.
	MonitorHtlcTimeoutSweep = fsm.StateType("MonitorHtlcTimeoutSweep")

	// HtlcTimeoutSwept is the state where the htlc timeout sweep
	// transaction was sufficiently confirmed.
	HtlcTimeoutSwept = fsm.StateType("HtlcTimeoutSwept")

	// SignSweeplessSweep is the state where the client signs the sweepless
	// sweep transaction.
	SignSweeplessSweep = fsm.StateType("SignSweeplessSweep")

	// Succeeded is the state the swap is in if it was successful.
	Succeeded = fsm.StateType("Succeeded")

	// SucceededSweeplessSigFailed is the state the swap is in if the swap
	// payment was received but the client failed to sign the sweepless
	// sweep transaction. This is considered a successful case from the
	// client's.
	SucceededSweeplessSigFailed = fsm.StateType("SucceededSweeplessSigFailed") //nolint:lll

	// ResetDeposits is the state where the deposits are reset. This happens
	// when the state machine encountered an error and the swap process
	// needs to start from the beginning.
	ResetDeposits = fsm.StateType("ResetDeposits")

	// Failed is the state the swap is in if it failed.
	Failed = fsm.StateType("Failed")
)

// Events.
var (
	OnInitHtlc                  = fsm.EventType("OnInitHtlc")
	OnHtlcInitiated             = fsm.EventType("OnHtlcInitiated")
	OnHtlcTxSigned              = fsm.EventType("OnHtlcTxSigned")
	OnSweepHtlcTimout           = fsm.EventType("OnSweepHtlcTimout")
	OnHtlcTimeoutSweepPublished = fsm.EventType("OnHtlcTimeoutSweepPublished")
	OnHtlcTimeoutSwept          = fsm.EventType("OnHtlcTimeoutSwept")
	OnPaymentReceived           = fsm.EventType("OnPaymentReceived")
	OnPaymentTimedOut           = fsm.EventType("OnPaymentTimedOut")
	OnSignSweeplessSweep        = fsm.EventType("OnSignSweeplessSweep")
	OnSweeplessSweepSigned      = fsm.EventType("OnSweeplessSweepSigned")
	OnRecover                   = fsm.EventType("OnRecover")
)

// LoopInStatesV0 returns the state and transition map for the loop-in state
// machine.
func (f *FSM) LoopInStatesV0() fsm.States {
	return fsm.States{
		fsm.EmptyState: fsm.State{
			Transitions: fsm.Transitions{
				OnInitHtlc: InitHtlc,
			},
			Action: fsm.NoOpAction,
		},
		InitHtlc: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcInitiated: SignHtlcTx,
				OnRecover:       ResetDeposits,
				fsm.OnError:     ResetDeposits,
			},
			Action: f.InitHtlcAction,
		},
		SignHtlcTx: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcTxSigned: MonitorInvoiceAndHtlcTx,
				OnRecover:      ResetDeposits,
				fsm.OnError:    ResetDeposits,
			},
			Action: f.SignHtlcTxAction,
		},
		MonitorInvoiceAndHtlcTx: fsm.State{
			Transitions: fsm.Transitions{
				OnPaymentReceived: PaymentReceived,
				OnSweepHtlcTimout: SweepHtlcTimout,
				OnPaymentTimedOut: ResetDeposits,
				OnRecover:         MonitorInvoiceAndHtlcTx,
				fsm.OnError:       ResetDeposits,
			},
			Action: f.MonitorInvoiceAndHtlcTxAction,
		},
		SweepHtlcTimout: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcTimeoutSweepPublished: MonitorHtlcTimeoutSweep,
				OnRecover:                   SweepHtlcTimout,
				fsm.OnError:                 Failed,
			},
			Action: f.SweepHtlcTimeoutAction,
		},
		MonitorHtlcTimeoutSweep: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcTimeoutSwept: HtlcTimeoutSwept,
				OnRecover:          MonitorHtlcTimeoutSweep,
				fsm.OnError:        Failed,
			},
			Action: f.MonitorHtlcTimeoutSweepAction,
		},
		PaymentReceived: fsm.State{
			Transitions: fsm.Transitions{
				OnSignSweeplessSweep: SignSweeplessSweep,
				OnRecover:            SucceededSweeplessSigFailed,
				fsm.OnError:          SucceededSweeplessSigFailed,
			},
			Action: f.PaymentReceivedAction,
		},
		SignSweeplessSweep: fsm.State{
			Transitions: fsm.Transitions{
				OnSweeplessSweepSigned: Succeeded,
				OnRecover:              SucceededSweeplessSigFailed,
				fsm.OnError:            SucceededSweeplessSigFailed,
			},
			Action: f.SignSweeplessSweepAction,
		},
		HtlcTimeoutSwept: fsm.State{
			Action: fsm.NoOpAction,
		},
		Succeeded: fsm.State{
			Action: fsm.NoOpAction,
		},
		SucceededSweeplessSigFailed: fsm.State{
			Action: fsm.NoOpAction,
		},
		ResetDeposits: fsm.State{
			Transitions: fsm.Transitions{
				OnRecover:   ResetDeposits,
				fsm.OnError: Failed,
			},
			Action: f.ResetDepositsAction,
		},
		Failed: fsm.State{
			Action: fsm.NoOpAction,
		},
	}
}

// updateLoopIn is called after every action and updates the loop-in in the db.
func (f *FSM) updateLoopIn(notification fsm.Notification) {
	f.Infof("Current: %v", notification.NextState)

	// Skip the update if the loop-in is not yet initialized.
	if f.loopIn == nil {
		return
	}

	f.loopIn.SetState(notification.NextState)

	// If we're in the early stages we don't have created the loop-in in the
	// store yet and won't need to update it. We also don't update if we're
	// doing a self transition.
	prevState := notification.PreviousState
	if f.loopIn.IsInState(fsm.EmptyState) ||
		f.loopIn.IsInState(prevState) ||
		prevState == fsm.EmptyState && f.loopIn.IsInState(InitHtlc) {

		return
	}

	err := f.cfg.Store.UpdateLoopIn(f.ctx, f.loopIn)
	if err != nil {
		log.Errorf("Error updating loop-in: %v", err)
		return
	}
}

// Infof logs an info message with the loop-in swap hash.
func (f *FSM) Infof(format string, args ...interface{}) {
	if f.loopIn == nil {
		log.Infof(format, args...)
		return
	}
	log.Infof(
		"StaticAddr loop-in %s: %s", f.loopIn.SwapHash.String(),
		fmt.Sprintf(format, args...),
	)
}

// Debugf logs a debug message with the loop-in swap hash.
func (f *FSM) Debugf(format string, args ...interface{}) {
	if f.loopIn == nil {
		log.Infof(format, args...)
		return
	}
	log.Debugf(
		"StaticAddr loop-in %s: %s", f.loopIn.SwapHash.String(),
		fmt.Sprintf(format, args...),
	)
}

// Errorf logs an error message with the loop-in swap hash.
func (f *FSM) Errorf(format string, args ...interface{}) {
	if f.loopIn == nil {
		log.Errorf(format, args...)
		return
	}
	log.Errorf(
		"StaticAddr loop-in %s: %s", f.loopIn.SwapHash.String(),
		fmt.Sprintf(format, args...),
	)
}
