package openchannel

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

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

type WithdrawalManager interface {
	CreateFinalizedWithdrawalTx(ctx context.Context,
		deposits []*deposit.Deposit, withdrawalAddress btcutil.Address,
		feeRate chainfee.SatPerKWeight,
		selectedWithdrawalAmount int64,
		commitmentType lnrpc.CommitmentType) (*wire.MsgTx, []byte, error)
}
