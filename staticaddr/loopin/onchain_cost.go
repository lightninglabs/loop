package loopin

import (
	"slices"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
)

// OnchainCostStatus tells whether the on-chain cost of a static loop-in is
// known.
type OnchainCostStatus uint8

const (
	// OnchainCostUnknown means the cost is unknown and zero: the swap
	// wasn't paid and isn't on its htlc timeout path, or the funding fee of
	// none of its deposits is known, for example if all of them were funded
	// by an external wallet.
	OnchainCostUnknown OnchainCostStatus = iota

	// OnchainCostPartial means the cost only includes the deposits whose
	// funding fee is known, and leaves out the others.
	OnchainCostPartial

	// OnchainCostComplete means the funding fees of all deposits are known
	// and included in the cost.
	OnchainCostComplete

	// OnchainCostPending means the cost of a deposit the swap spent hasn't
	// been determined yet, so the cost only includes the deposits whose
	// funding fee is known so far.
	OnchainCostPending
)

// OnchainCost is the client-side on-chain cost of a static loop-in.
type OnchainCost struct {
	// Amount is the sum of the funding fee shares of the swap's deposits
	// whose funding fee the wallet knows.
	Amount btcutil.Amount

	// Status tells whether Amount includes the funding fees of all, some
	// or none of the deposits.
	Status OnchainCostStatus
}

// paidStates are the loop-in states in which the swap invoice was paid, so the
// swap spent its deposits.
var paidStates = []fsm.StateType{
	PaymentReceived, Succeeded, SucceededTransitioningFailed,
}

// htlcTimeoutStates are the loop-in states in which the swap wasn't paid but
// its htlc confirmed and timed out, so its deposits can only be swept through
// the htlc timeout path.
var htlcTimeoutStates = []fsm.StateType{
	SweepHtlcTimeout, MonitorHtlcTimeoutSweep, HtlcTimeoutSwept,
}

// htlcTimeoutDepositStates are the deposit states of deposits spent by an htlc
// that confirmed.
var htlcTimeoutDepositStates = []fsm.StateType{
	deposit.SweepHtlcTimeout, deposit.HtlcTimeoutSwept,
}

// SwapOnchainCost returns the on-chain cost of a static loop-in: the sum of
// the on-chain costs of the deposits it spent. The given deposits are the ones
// mapped to the swap. Each deposit is mapped to the last swap that used it, so
// a deposit that a failed attempt gave back and a later swap spent only counts
// for the later swap.
//
// All deposits count if the swap was paid. A swap that wasn't paid only counts
// the deposits on the htlc timeout path, and only once it is on its own htlc
// timeout path, or failed after publishing its own htlc timeout sweep. Before
// that, deposits mapped to it can be on the htlc timeout path because of
// another swap's htlc: the deposit swap hash migration mapped a deposit to the
// first swap that used it, and a swap can take back deposits that a later swap
// had already taken. The published sweep is the best signal that a failed
// swap's own htlc spent its deposits: a swap whose sweep couldn't be recorded
// reports an unknown cost.
func SwapOnchainCost(swp *StaticAddressLoopIn,
	deposits []*deposit.Deposit) OnchainCost {

	state := swp.GetState()
	paid := slices.Contains(paidStates, state)
	ownHtlcTimeout := slices.Contains(htlcTimeoutStates, state) ||
		(state == Failed && swp.HtlcTimeoutSweepTxHash != nil)

	if !paid && !ownHtlcTimeout {
		return OnchainCost{Status: OnchainCostUnknown}
	}

	var (
		amount  btcutil.Amount
		spent   int
		known   int
		unknown int
	)
	for _, d := range deposits {
		onHtlcPath := slices.Contains(
			htlcTimeoutDepositStates, d.GetState(),
		)
		if !paid && !onHtlcPath {
			continue
		}
		spent++

		cost := d.GetOnchainCost()
		switch cost.Status {
		case deposit.OnchainCostKnown:
			amount += cost.Amount
			known++

		case deposit.OnchainCostUnknown:
			unknown++
		}
	}

	switch {
	// The swap didn't spend any deposits.
	case spent == 0:
		return OnchainCost{Status: OnchainCostUnknown}

	// The cost of a deposit hasn't been determined yet.
	case known+unknown < spent:
		return OnchainCost{Amount: amount, Status: OnchainCostPending}

	case unknown == 0:
		return OnchainCost{Amount: amount, Status: OnchainCostComplete}

	case known == 0:
		return OnchainCost{Status: OnchainCostUnknown}

	default:
		return OnchainCost{Amount: amount, Status: OnchainCostPartial}
	}
}
