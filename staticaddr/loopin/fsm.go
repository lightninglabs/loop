package loopin

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/version"
)

// FSM embeds an FSM and extends it with a static address loop-in and a config.
type FSM struct {
	*fsm.StateMachine

	cfg *Config

	// loopIn stores the loop-in details that are relevant during the
	// lifetime of the swap.
	loopIn *StaticAddressLoopIn

	// MuSig2 data must not be re-used across restarts, hence it is not
	// persisted.
	//
	// htlcServerNonces contains all the nonces that the server generated
	// for the htlc musig2 sessions.
	htlcServerNonces [][musig2.PubNonceSize]byte

	// htlcServerNoncesHighFee contains all the high fee nonces that the
	// server generated for the htlc musig2 sessions.
	htlcServerNoncesHighFee [][musig2.PubNonceSize]byte

	// htlcServerNoncesExtremelyHighFee contains all the extremely high fee
	// nonces that the  server generated for the htlc musig2 sessions.
	htlcServerNoncesExtremelyHighFee [][musig2.PubNonceSize]byte
}

// NewFSM creates a new loop-in state machine.
func NewFSM(loopIn *StaticAddressLoopIn, cfg *Config, recoverFsm bool) (*FSM,
	error) {

	loopInFsm := &FSM{
		cfg:    cfg,
		loopIn: loopIn,
	}

	for _, d := range loopIn.Deposits {
		switch d.AddressParams.ProtocolVersion {
		case version.ProtocolVersion_V0:

		default:
			return nil, deposit.ErrProtocolVersionNotSupported
		}
	}

	loopInStates := loopInFsm.LoopInStatesV0()
	if recoverFsm {
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
	// InitHtlcTx initiates the htlc tx creation with the server.
	InitHtlcTx = fsm.StateType("InitHtlcTx")

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

	// SweepHtlcTimeout is the state where the htlc timeout path is
	// published because the server did not pay the invoice on time.
	SweepHtlcTimeout = fsm.StateType("SweepHtlcTimeout")

	// MonitorHtlcTimeoutSweep monitors the htlc timeout sweep transaction
	// confirmation.
	MonitorHtlcTimeoutSweep = fsm.StateType("MonitorHtlcTimeoutSweep")

	// HtlcTimeoutSwept is the state where the htlc timeout sweep
	// transaction was sufficiently confirmed.
	HtlcTimeoutSwept = fsm.StateType("HtlcTimeoutSwept")

	// Succeeded is the state the swap is in if it was successful.
	Succeeded = fsm.StateType("Succeeded")

	// SucceededTransitioningFailed is the state the swap is in if the swap
	// payment was received but the client was not able to transition
	// the deposits to the looped-in state.
	SucceededTransitioningFailed = fsm.StateType("SucceededTransitioningFailed") //nolint:lll

	// UnlockDeposits is the state where the deposits are reset. This
	// happens when the state machine encountered an error and the swap
	// process needs to start from the beginning.
	UnlockDeposits = fsm.StateType("UnlockDeposits")

	// Failed is the state the swap is in if it failed.
	Failed = fsm.StateType("Failed")
)

var PendingStates = []fsm.StateType{
	InitHtlcTx, SignHtlcTx, MonitorInvoiceAndHtlcTx, PaymentReceived,
	SweepHtlcTimeout, MonitorHtlcTimeoutSweep, UnlockDeposits,
}

var FinalStates = []fsm.StateType{
	HtlcTimeoutSwept, Succeeded, SucceededTransitioningFailed, Failed,
}

var AllStates = append(
	append([]fsm.StateType{}, PendingStates...), FinalStates...,
)

// Events.
var (
	OnInitHtlc                  = fsm.EventType("OnInitHtlc")
	OnHtlcInitiated             = fsm.EventType("OnHtlcInitiated")
	OnHtlcTxSigned              = fsm.EventType("OnHtlcTxSigned")
	OnSweepHtlcTimeout          = fsm.EventType("OnSweepHtlcTimeout")
	OnHtlcTimeoutSweepPublished = fsm.EventType("OnHtlcTimeoutSweepPublished")
	OnHtlcTimeoutSwept          = fsm.EventType("OnHtlcTimeoutSwept")
	OnPaymentReceived           = fsm.EventType("OnPaymentReceived")
	OnSwapTimedOut              = fsm.EventType("OnSwapTimedOut")
	OnSucceeded                 = fsm.EventType("OnSucceeded")
	OnRecover                   = fsm.EventType("OnRecover")
)

// LoopInStatesV0 returns the state and transition map for the loop-in state
// machine.
func (f *FSM) LoopInStatesV0() fsm.States {
	return fsm.States{
		fsm.EmptyState: fsm.State{
			Transitions: fsm.Transitions{
				OnInitHtlc: InitHtlcTx,
			},
			Action: fsm.NoOpAction,
		},
		InitHtlcTx: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcInitiated: SignHtlcTx,
				OnRecover:       UnlockDeposits,
				fsm.OnError:     UnlockDeposits,
			},
			Action: f.InitHtlcAction,
		},
		SignHtlcTx: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcTxSigned: MonitorInvoiceAndHtlcTx,
				OnRecover:      UnlockDeposits,
				fsm.OnError:    UnlockDeposits,
			},
			Action: f.SignHtlcTxAction,
		},
		MonitorInvoiceAndHtlcTx: fsm.State{
			Transitions: fsm.Transitions{
				OnPaymentReceived:  PaymentReceived,
				OnSweepHtlcTimeout: SweepHtlcTimeout,
				OnSwapTimedOut:     Failed,
				OnRecover:          MonitorInvoiceAndHtlcTx,
				fsm.OnError:        UnlockDeposits,
			},
			Action: f.MonitorInvoiceAndHtlcTxAction,
		},
		SweepHtlcTimeout: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcTimeoutSweepPublished: MonitorHtlcTimeoutSweep,
				OnRecover:                   SweepHtlcTimeout,
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
				OnSucceeded: Succeeded,
				OnRecover:   Succeeded,
				fsm.OnError: SucceededTransitioningFailed,
			},
			Action: f.PaymentReceivedAction,
		},
		HtlcTimeoutSwept: fsm.State{
			Action: fsm.NoOpAction,
		},
		Succeeded: fsm.State{
			Action: fsm.NoOpAction,
		},
		SucceededTransitioningFailed: fsm.State{
			Action: fsm.NoOpAction,
		},
		UnlockDeposits: fsm.State{
			Transitions: fsm.Transitions{
				OnRecover:   UnlockDeposits,
				fsm.OnError: Failed,
			},
			Action: f.UnlockDepositsAction,
		},
		Failed: fsm.State{
			Action: fsm.NoOpAction,
		},
	}
}

// updateLoopIn is called after every action and updates the loop-in in the db.
func (f *FSM) updateLoopIn(ctx context.Context, notification fsm.Notification) {
	f.Infof("Current: %v", notification.NextState)

	// Skip the update if the loop-in is not yet initialized. This happens
	// on the entry action of the fsm.
	if f.loopIn == nil {
		return
	}

	f.loopIn.SetState(notification.NextState)

	// Check if we can skip updating the loop-in in the database.
	if isUpdateSkipped(notification, f.loopIn) {
		return
	}

	stored, err := f.cfg.Store.IsStored(ctx, f.loopIn.SwapHash)
	if err != nil {
		f.Errorf("Error checking if loop-in is stored: %v", err)

		return
	}

	if !stored {
		f.Warnf("Loop-in not stored in db, can't update")

		return
	}

	err = f.cfg.Store.UpdateLoopIn(ctx, f.loopIn)
	if err != nil {
		f.Errorf("Error updating loop-in: %v", err)

		return
	}
}

// isUpdateSkipped returns true if the loop-in should not be updated for the
// given notification.
func isUpdateSkipped(notification fsm.Notification,
	l *StaticAddressLoopIn) bool {

	prevState := notification.PreviousState

	// Skip if we are in the empty state because no loop-in has been
	// persisted yet.
	if l.IsInState(fsm.EmptyState) {
		return true
	}

	// We don't update in self-loops, e.g. in the case of recovery.
	if l.IsInState(prevState) {
		return true
	}

	// If we transitioned from the empty state to InitHtlcTx there's still
	// no loop-in persisted, so we don't need to update it.
	if prevState == fsm.EmptyState && l.IsInState(InitHtlcTx) {
		return true
	}

	return false
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
		log.Debugf(format, args...)
		return
	}
	log.Debugf(
		"StaticAddr loop-in %s: %s", f.loopIn.SwapHash.String(),
		fmt.Sprintf(format, args...),
	)
}

// Warnf logs a warning message with the loop-in swap hash.
func (f *FSM) Warnf(format string, args ...interface{}) {
	if f.loopIn == nil {
		log.Warnf(format, args...)
		return
	}
	log.Warnf(
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
