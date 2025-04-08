package withdraw

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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
	// GetStaticAddressParameters returns the static address parameters.
	GetStaticAddressParameters(ctx context.Context) (*address.Parameters,
		error)

	// GetStaticAddress returns the deposit address for the given
	// client and server public keys.
	GetStaticAddress(ctx context.Context) (*script.StaticAddress, error)
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

// Estimator is an interface that allows us to estimate the fee rate in sat/kw.
type Estimator interface {
	// EstimateFeeRate estimates the fee rate in sat/kw for a transaction to
	// be confirmed in the given number of blocks.
	EstimateFeeRate(ctx context.Context, target int32) (
		chainfee.SatPerKWeight, error)
}
