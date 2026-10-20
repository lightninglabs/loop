// Package flow owns the client static loop-in transition policy. Bitcoin and
// server-executed asset swaps bind different effects to the same lifecycle.
package flow

import "errors"

// Protocol selects who controls execution and the corresponding recovery rules.
type Protocol uint8

const (
	BitcoinV0 Protocol = iota
	ServerAsset
)

// State is a persisted static loop-in lifecycle state.
type State string

const (
	Empty                        State = ""
	InitHtlcTx                   State = "InitHtlcTx"
	SignHtlcTx                   State = "SignHtlcTx"
	MonitorInvoiceAndHtlcTx      State = "MonitorInvoiceAndHtlcTx"
	PaymentReceived              State = "PaymentReceived"
	SweepHtlcTimeout             State = "SweepHtlcTimeout"
	MonitorHtlcTimeoutSweep      State = "MonitorHtlcTimeoutSweep"
	HtlcTimeoutSwept             State = "HtlcTimeoutSwept"
	Succeeded                    State = "Succeeded"
	SucceededTransitioningFailed State = "SucceededTransitioningFailed"
	UnlockDeposits               State = "UnlockDeposits"
	Failed                       State = "Failed"
)

// Terminal reports that the protocol has reached its final lifecycle state.
// A separate durable cleanup marker can still require deposit-manager recovery.
func (s State) Terminal() bool {
	return s == HtlcTimeoutSwept || s == Succeeded ||
		s == SucceededTransitioningFailed || s == Failed
}

// Event identifies a validated outcome of a protocol-specific action.
type Event string

const (
	OnInitHtlc                  Event = "OnInitHtlc"
	OnHtlcInitiated             Event = "OnHtlcInitiated"
	OnHtlcTxSigned              Event = "OnHtlcTxSigned"
	OnPaymentReceived           Event = "OnPaymentReceived"
	OnSweepHtlcTimeout          Event = "OnSweepHtlcTimeout"
	OnHtlcTimeoutSweepPublished Event = "OnHtlcTimeoutSweepPublished"
	OnHtlcTimeoutSwept          Event = "OnHtlcTimeoutSwept"
	OnSwapTimedOut              Event = "OnSwapTimedOut"
	OnSucceeded                 Event = "OnSucceeded"
	OnCanceled                  Event = "OnCanceled"
	OnRecover                   Event = "OnRecover"
	OnError                     Event = "OnError"
)

// Graph contains only transitions, never signing, payment, or storage effects.
type Graph map[State]map[Event]State

// ErrTransition rejects an event outside the selected protocol's graph.
var ErrTransition = errors.New("invalid loop-in transition")

// States returns an independent graph. Asset effects must persist their full
// evidence before exposure; a generic FSM entry hook is not a durable journal.
func States(protocol Protocol) Graph {
	if protocol == ServerAsset {
		return Graph{
			Empty: {OnInitHtlc: InitHtlcTx},
			InitHtlcTx: {
				OnHtlcInitiated: SignHtlcTx, OnCanceled: Failed,
				OnRecover: InitHtlcTx,
			},
			SignHtlcTx: {
				OnHtlcTxSigned: MonitorInvoiceAndHtlcTx,
				OnCanceled:     Failed, OnRecover: SignHtlcTx,
			},
			MonitorInvoiceAndHtlcTx: {
				OnPaymentReceived:  PaymentReceived,
				OnSweepHtlcTimeout: SweepHtlcTimeout,
				OnRecover:          MonitorInvoiceAndHtlcTx,
			},
			SweepHtlcTimeout: {
				OnPaymentReceived:  PaymentReceived,
				OnHtlcTimeoutSwept: HtlcTimeoutSwept,
				OnSweepHtlcTimeout: SweepHtlcTimeout,
				OnRecover:          SweepHtlcTimeout,
			},
			PaymentReceived: {
				OnSucceeded:       Succeeded,
				OnRecover:         PaymentReceived,
				OnPaymentReceived: PaymentReceived,
			},
			HtlcTimeoutSwept: {}, Succeeded: {}, Failed: {},
		}
	}
	if protocol != BitcoinV0 {
		return nil
	}
	return Graph{
		Empty: {OnInitHtlc: InitHtlcTx},
		InitHtlcTx: {
			OnHtlcInitiated: SignHtlcTx,
			OnRecover:       UnlockDeposits,
			OnError:         UnlockDeposits,
		},
		SignHtlcTx: {
			OnHtlcTxSigned: MonitorInvoiceAndHtlcTx,
			OnRecover:      UnlockDeposits, OnError: UnlockDeposits,
		},
		MonitorInvoiceAndHtlcTx: {
			OnPaymentReceived:  PaymentReceived,
			OnSweepHtlcTimeout: SweepHtlcTimeout,
			OnSwapTimedOut:     Failed,
			OnRecover:          MonitorInvoiceAndHtlcTx,
			OnError:            UnlockDeposits,
		},
		SweepHtlcTimeout: {
			OnHtlcTimeoutSweepPublished: MonitorHtlcTimeoutSweep,
			OnRecover:                   SweepHtlcTimeout,
			OnError:                     Failed,
		},
		MonitorHtlcTimeoutSweep: {
			OnHtlcTimeoutSwept: HtlcTimeoutSwept,
			OnRecover:          MonitorHtlcTimeoutSweep,
			OnError:            Failed,
		},
		PaymentReceived: {
			OnSucceeded: Succeeded, OnRecover: Succeeded,
			OnError: SucceededTransitioningFailed,
		},
		UnlockDeposits:   {OnRecover: UnlockDeposits, OnError: Failed},
		HtlcTimeoutSwept: {}, Succeeded: {},
		SucceededTransitioningFailed: {}, Failed: {},
	}
}

// Next resolves one event without changing state or performing effects.
func Next(protocol Protocol, state State, event Event) (State, error) {
	next, ok := States(protocol)[state][event]
	if !ok {
		return state, ErrTransition
	}
	return next, nil
}

// Allows validates persisted state changes against the shared transition map.
// Self-updates retain write-ahead evidence within a known lifecycle state.
func Allows(protocol Protocol, from, to State) bool {
	transitions, ok := States(protocol)[from]
	if !ok {
		return false
	}
	if from == to {
		return true
	}
	for _, target := range transitions {
		if target == to {
			return true
		}
	}
	return false
}
