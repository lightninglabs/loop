package loopin

import (
	"context"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
)

// AddressManager handles fetching of address parameters.
type AddressManager interface {
	// GetStaticAddressParameters returns the static address parameters.
	GetStaticAddressParameters(ctx context.Context) (*address.Parameters,
		error)

	// GetStaticAddress returns the deposit address for the given client and
	// server public keys.
	GetStaticAddress(ctx context.Context) (*script.StaticAddress, error)
}

// DepositManager handles the interaction of loop-ins with deposits.
type DepositManager interface {
	// AllStringOutpointsActiveDeposits returns all deposits that have the
	// given outpoints and are in the given state.
	AllStringOutpointsActiveDeposits(outpoints []string,
		stateFilter fsm.StateType) ([]*deposit.Deposit, bool)

	// TransitionDeposits transitions the given deposits to the next state
	// based on the given event. It returns an error if the transition is
	// invalid.
	TransitionDeposits(deposits []*deposit.Deposit, event fsm.EventType,
		expectedFinalState fsm.StateType) error
}

// StaticAddressLoopInStore provides access to the static address loop-in DB.
type StaticAddressLoopInStore interface {
	// CreateLoopIn creates a loop-in record in the database.
	CreateLoopIn(ctx context.Context, loopIn *StaticAddressLoopIn) error

	// UpdateLoopIn updates a loop-in record in the database.
	UpdateLoopIn(ctx context.Context, loopIn *StaticAddressLoopIn) error

	// AllLoopIns returns all loop-ins from the database.
	AllLoopIns(ctx context.Context) ([]*StaticAddressLoopIn, error)
}
