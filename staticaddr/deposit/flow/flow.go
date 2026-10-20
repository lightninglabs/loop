// Package flow defines shared deposit lifecycle transitions without wallet
// or signer dependencies. Callers persist ownership and verify evidence before
// applying events; a graph transition alone cannot reserve or spend an asset.
package flow

import "errors"

// State is the persisted deposit state name.
type State string

// Event is a deposit lifecycle transition trigger.
type Event string

// Graph maps each state and accepted event to its next state.
type Graph map[State]map[Event]State

// Protocol selects the execution and recovery policy.
type Protocol uint8

const (
	// Bitcoin preserves the existing static-address deposit lifecycle.
	Bitcoin Protocol = iota
	// ServerAsset keeps exposed assets locked for proof-based recovery.
	ServerAsset
)

// Existing persisted state encodings are unchanged.
const (
	EmptyState         State = ""
	Deposited          State = "Deposited"
	PublishExpirySweep State = "PublishExpirySweep"
	WaitForExpirySweep State = "WaitForExpirySweep"
	Expired            State = "Expired"
	Withdrawing        State = "Withdrawing"
	LoopingIn          State = "LoopingIn"
	LoopedIn           State = "LoopedIn"
	SweepHtlcTimeout   State = "SweepHtlcTimeout"
	HtlcTimeoutSwept   State = "HtlcTimeoutSwept"
	Withdrawn          State = "Withdrawn"
	OpeningChannel     State = "OpeningChannel"
	ChannelPublished   State = "ChannelPublished"
	AssetClaimExposed  State = "AssetClaimExposed"
)

// Events retain the existing static-address names.
const (
	OnChannelPublished    Event = "OnChannelPublished"
	OnError               Event = "OnError"
	OnExpiry              Event = "OnExpiry"
	OnExpiryPublished     Event = "OnExpiryPublished"
	OnExpirySwept         Event = "OnExpirySwept"
	OnHtlcTimeoutSwept    Event = "OnHtlcTimeoutSwept"
	OnLoopInInitiated     Event = "OnLoopInInitiated"
	OnLoopedIn            Event = "OnLoopedIn"
	OnOpeningChannel      Event = "OnOpeningChannel"
	OnRecover             Event = "OnRecover"
	OnStart               Event = "OnStart"
	OnSweepingHtlcTimeout Event = "OnSweepingHtlcTimeout"
	OnWithdrawInitiated   Event = "OnWithdrawInitiated"
	OnWithdrawn           Event = "OnWithdrawn"
	OnClaimExposed        Event = "OnClaimExposed"
)

// ErrTransition rejects unknown protocols, states and events.
var ErrTransition = errors.New("deposit transition rejected")

// States returns a fresh transition graph for the chosen protocol.
func States(protocol Protocol) Graph {
	if protocol != Bitcoin && protocol != ServerAsset {
		return nil
	}
	graph := Graph{
		EmptyState: {
			OnStart: Deposited,
		},
		Deposited: {
			OnExpiry:              PublishExpirySweep,
			OnWithdrawInitiated:   Withdrawing,
			OnLoopInInitiated:     LoopingIn,
			OnOpeningChannel:      OpeningChannel,
			OnSweepingHtlcTimeout: SweepHtlcTimeout,
			OnRecover:             Deposited,
			OnError:               Deposited,
		},
		PublishExpirySweep: {
			OnRecover:         PublishExpirySweep,
			OnExpiryPublished: WaitForExpirySweep,
			OnError:           Deposited,
		},
		WaitForExpirySweep: {
			OnExpirySwept: Expired,
			OnRecover:     PublishExpirySweep,
			OnError:       Deposited,
		},
		Expired: {
			OnExpiry: Expired,
		},
		Withdrawing: {
			OnWithdrawn:         Withdrawn,
			OnWithdrawInitiated: Withdrawing,
			OnRecover:           Withdrawing,
			OnExpiry:            Withdrawing,
			OnError:             Deposited,
		},
		LoopingIn: {
			OnLoopedIn:            LoopedIn,
			OnExpiry:              PublishExpirySweep,
			OnSweepingHtlcTimeout: SweepHtlcTimeout,
			OnLoopInInitiated:     LoopingIn,
			OnRecover:             LoopingIn,
			OnError:               Deposited,
		},
		LoopedIn: {
			OnExpiry: LoopedIn,
		},
		SweepHtlcTimeout: {
			OnHtlcTimeoutSwept: HtlcTimeoutSwept,
			OnRecover:          SweepHtlcTimeout,
		},
		HtlcTimeoutSwept: {
			OnExpiry: HtlcTimeoutSwept,
		},
		Withdrawn: {
			OnExpiry:    Withdrawn,
			OnWithdrawn: Withdrawn,
		},
		OpeningChannel: {
			OnError:            Deposited,
			OnChannelPublished: ChannelPublished,
			OnRecover:          OpeningChannel,
			OnExpiry:           OpeningChannel,
		},
		ChannelPublished: {
			OnExpiry: ChannelPublished,
		},
	}
	if protocol == Bitcoin {
		return graph
	}

	// Asset outputs require proof-preserving recovery. Exclude actions
	// that spend Bitcoin without producing the corresponding asset proof.
	for state := range graph {
		switch state {
		case EmptyState, Deposited, LoopingIn, LoopedIn,
			SweepHtlcTimeout, HtlcTimeoutSwept:
		default:
			delete(graph, state)
		}
	}
	for event := range graph[Deposited] {
		switch event {
		case OnLoopInInitiated, OnRecover, OnError:
		default:
			delete(graph[Deposited], event)
		}
	}
	graph[Deposited][OnExpiry] = Deposited
	delete(graph[LoopingIn], OnLoopedIn)
	delete(graph[LoopingIn], OnSweepingHtlcTimeout)
	graph[LoopingIn][OnExpiry] = LoopingIn
	graph[LoopingIn][OnClaimExposed] = AssetClaimExposed
	graph[AssetClaimExposed] = map[Event]State{
		OnClaimExposed:        AssetClaimExposed,
		OnLoopedIn:            LoopedIn,
		OnSweepingHtlcTimeout: SweepHtlcTimeout,
		OnRecover:             AssetClaimExposed,
		OnError:               AssetClaimExposed,
		OnExpiry:              AssetClaimExposed,
	}
	// Settlement and a confirmed server claim can win the refund race.
	graph[SweepHtlcTimeout][OnLoopedIn] = LoopedIn
	graph[SweepHtlcTimeout][OnError] = SweepHtlcTimeout
	graph[SweepHtlcTimeout][OnExpiry] = SweepHtlcTimeout
	graph[LoopedIn][OnRecover] = LoopedIn
	graph[HtlcTimeoutSwept][OnRecover] = HtlcTimeoutSwept
	return graph
}

// Next validates one transition without performing or persisting any effects.
func Next(protocol Protocol, state State, event Event) (State, error) {
	next, ok := States(protocol)[state][event]
	if !ok {
		return state, ErrTransition
	}
	return next, nil
}

// IsFinal reports states with a completed on-chain disposition.
func IsFinal(state State) bool {
	return state == Expired || state == Withdrawn || state == LoopedIn ||
		state == HtlcTimeoutSwept || state == ChannelPublished
}
