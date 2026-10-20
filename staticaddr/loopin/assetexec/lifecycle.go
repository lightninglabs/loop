package assetexec

import (
	depositflow "github.com/lightninglabs/loop/staticaddr/deposit/flow"
	"github.com/lightninglabs/loop/staticaddr/loopin/flow"
)

// transition applies a shared loop-in event and the corresponding deposit
// event to the private record. The caller must persist it before any effect.
func transition(r *Record, event flow.Event) error {
	next, err := flow.Next(flow.ServerAsset, r.State, event)
	if err != nil {
		return ErrState
	}
	var depositEvent depositflow.Event
	switch event {
	case flow.OnHtlcTxSigned:
		depositEvent = depositflow.OnClaimExposed
	case flow.OnSweepHtlcTimeout:
		if r.DepositState != depositflow.SweepHtlcTimeout {
			depositEvent = depositflow.OnSweepingHtlcTimeout
		}
	case flow.OnHtlcTimeoutSwept:
		depositEvent = depositflow.OnHtlcTimeoutSwept
	case flow.OnSucceeded:
		depositEvent = depositflow.OnLoopedIn
	case flow.OnCanceled:
		depositEvent = depositflow.OnError
	}
	if depositEvent != "" {
		if err := transitionDeposit(r, depositEvent); err != nil {
			return err
		}
	}
	r.State = next
	return nil
}

func transitionDeposit(r *Record, event depositflow.Event) error {
	next, err := depositflow.Next(
		depositflow.ServerAsset, r.DepositState, event,
	)
	if err != nil {
		return ErrState
	}
	r.DepositState = next
	return nil
}

// inferredDepositState upgrades journals written before deposit lifecycle
// state was explicit. Existing durable evidence determines the safe state;
// an exposure marker always prevents interpreting a deposit as available.
func inferredDepositState(r *Record) depositflow.State {
	switch {
	case r.State == Succeeded:
		return depositflow.LoopedIn
	case r.State == HtlcTimeoutSwept:
		return depositflow.HtlcTimeoutSwept
	case r.State == Failed:
		return depositflow.Deposited
	case r.CancellationRequested && r.SignaturesExposed:
		return depositflow.SweepHtlcTimeout
	case r.SignaturesExposed:
		return depositflow.AssetClaimExposed
	case r.Reserved:
		return depositflow.LoopingIn
	default:
		return depositflow.Deposited
	}
}
