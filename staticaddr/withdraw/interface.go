package withdraw

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
)

// Store is the database interface that is used to store and retrieve
// static address withdrawals.
type Store interface {
	// CreateWithdrawal inserts a withdrawal into the store.
	CreateWithdrawal(ctx context.Context, tx *wire.MsgTx,
		confirmationHeight uint32, deposits []*deposit.Deposit,
		changePkScript []byte) error

	// GetAllWithdrawals retrieves all withdrawals.
	GetAllWithdrawals(ctx context.Context) ([]Withdrawal, error)
}

// AddressManager handles fetching of address parameters.
type AddressManager interface {
	// NewAddress returns a new static address.
	NewAddress(ctx context.Context) (*address.Parameters, error)
}

type DepositManager interface {
	GetActiveDepositsInState(stateFilter fsm.StateType) ([]*deposit.Deposit,
		error)

	AllOutpointsActiveDeposits(outpoints []wire.OutPoint,
		stateFilter fsm.StateType) ([]*deposit.Deposit, bool)

	TransitionDeposits(ctx context.Context, deposits []*deposit.Deposit,
		event fsm.EventType, expectedFinalState fsm.StateType) error

	UpdateDeposit(ctx context.Context, d *deposit.Deposit) error
}
