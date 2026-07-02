package withdraw

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
)

// AddressManager handles fetching of address parameters.
type AddressManager interface {
	// GetStaticAddressParameters returns the static address parameters.
	GetStaticAddressParameters(ctx context.Context) (*script.Parameters,
		error)

	// GetStaticAddress returns the deposit address for the given
	// client and server public keys.
	GetStaticAddress(ctx context.Context) (*script.StaticAddress, error)
}

type DepositManager interface {
	// EnsureDepositsFresh reconciles active deposits with the wallet view.
	EnsureDepositsFresh(ctx context.Context) error

	// GetActiveDepositsInState returns all active deposits in the given
	// state.
	GetActiveDepositsInState(stateFilter fsm.StateType) ([]*deposit.Deposit,
		error)

	// AllOutpointsActiveDeposits returns all active deposits referenced by
	// the outpoints if every deposit is active and in the given state.
	AllOutpointsActiveDeposits(outpoints []wire.OutPoint,
		stateFilter fsm.StateType) ([]*deposit.Deposit, bool)

	// TransitionDeposits transitions the deposits with the given event and
	// waits until they reach the expected final state.
	TransitionDeposits(ctx context.Context, deposits []*deposit.Deposit,
		event fsm.EventType, expectedFinalState fsm.StateType) error

	// UpdateDeposit persists the current deposit fields.
	UpdateDeposit(ctx context.Context, d *deposit.Deposit) error
}
