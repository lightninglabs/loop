package openchannel

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// Estimator is an interface that allows us to estimate the fee rate in sat/kw.
type Estimator interface {
	// EstimateFeeRate estimates the fee rate in sat/kw for a transaction to
	// be confirmed in the given number of blocks.
	EstimateFeeRate(ctx context.Context, target int32) (
		chainfee.SatPerKWeight, error)
}

// AddressManager handles fetching of address parameters.
type AddressManager interface {
	// NewAddress returns a new static address.
	NewAddress(ctx context.Context) (*address.Parameters, error)
}

type DepositManager interface {
	// AllOutpointsActiveDeposits returns all deposits that are in the
	// given state. If the state filter is fsm.StateTypeNone, all deposits
	// are returned.
	AllOutpointsActiveDeposits(outpoints []wire.OutPoint,
		stateFilter fsm.StateType) ([]*deposit.Deposit, bool)

	// GetActiveDepositsInState returns all deposits that are in the
	// given state.
	GetActiveDepositsInState(stateFilter fsm.StateType) ([]*deposit.Deposit,
		error)

	// TransitionDeposits transitions the deposits to the given state.
	TransitionDeposits(ctx context.Context, deposits []*deposit.Deposit,
		event fsm.EventType, expectedFinalState fsm.StateType) error
}
