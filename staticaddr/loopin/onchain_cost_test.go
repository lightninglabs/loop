package loopin

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/stretchr/testify/require"
)

// costDeposit returns a deposit in the given state with the given on-chain
// cost.
func costDeposit(state fsm.StateType,
	cost deposit.OnchainCost) *deposit.Deposit {

	d := &deposit.Deposit{OnchainCost: cost}
	d.SetState(state)

	return d
}

// TestSwapOnchainCost tests which deposits count for the on-chain cost of a
// static loop-in, and the cost's status.
func TestSwapOnchainCost(t *testing.T) {
	known := func(amount btcutil.Amount) deposit.OnchainCost {
		return deposit.OnchainCost{
			Amount: amount,
			Status: deposit.OnchainCostKnown,
		}
	}
	unknown := deposit.OnchainCost{Status: deposit.OnchainCostUnknown}
	undetermined := deposit.OnchainCost{}

	tests := []struct {
		name     string
		state    fsm.StateType
		sweepTx  bool
		deposits []*deposit.Deposit
		cost     OnchainCost
	}{
		{
			name:  "pending swap",
			state: MonitorInvoiceAndHtlcTx,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopingIn, known(900)),
			},
			cost: OnchainCost{Status: OnchainCostUnknown},
		},
		{
			// The deposits are on the htlc timeout path while the
			// swap still waits for its payment. Either its htlc
			// confirmed, or an earlier swap's htlc spent deposits
			// that this swap had taken, so they don't count yet.
			name:  "htlc deposits, swap not timed out",
			state: MonitorInvoiceAndHtlcTx,
			deposits: []*deposit.Deposit{
				costDeposit(
					deposit.SweepHtlcTimeout, known(900),
				),
			},
			cost: OnchainCost{Status: OnchainCostUnknown},
		},
		{
			name:  "htlc deposits, swap unlocking deposits",
			state: UnlockDeposits,
			deposits: []*deposit.Deposit{
				costDeposit(
					deposit.SweepHtlcTimeout, known(900),
				),
			},
			cost: OnchainCost{Status: OnchainCostUnknown},
		},
		{
			name:  "htlc deposits, htlc not signed yet",
			state: SignHtlcTx,
			deposits: []*deposit.Deposit{
				costDeposit(
					deposit.SweepHtlcTimeout, known(900),
				),
			},
			cost: OnchainCost{Status: OnchainCostUnknown},
		},
		{
			name:  "htlc deposits, htlc not initiated yet",
			state: InitHtlcTx,
			deposits: []*deposit.Deposit{
				costDeposit(
					deposit.SweepHtlcTimeout, known(900),
				),
			},
			cost: OnchainCost{Status: OnchainCostUnknown},
		},
		{
			// Only the deposit on the htlc timeout path counts for
			// a swap that wasn't paid.
			name:  "sweeping, one deposit off the path",
			state: SweepHtlcTimeout,
			deposits: []*deposit.Deposit{
				costDeposit(
					deposit.SweepHtlcTimeout, known(900),
				),
				costDeposit(deposit.Deposited, known(300)),
			},
			cost: OnchainCost{
				Amount: 900, Status: OnchainCostComplete,
			},
		},
		{
			name:  "payment received",
			state: PaymentReceived,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopingIn, known(900)),
			},
			cost: OnchainCost{
				Amount: 900, Status: OnchainCostComplete,
			},
		},
		{
			name:  "succeeded",
			state: Succeeded,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopedIn, known(900)),
				costDeposit(deposit.LoopedIn, known(300)),
			},
			cost: OnchainCost{
				Amount: 1_200, Status: OnchainCostComplete,
			},
		},
		{
			// The payment was received but the deposits couldn't
			// be moved to LoopedIn. They are still spent.
			name:  "paid, deposits not finalized",
			state: SucceededTransitioningFailed,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopingIn, known(900)),
			},
			cost: OnchainCost{
				Amount: 900, Status: OnchainCostComplete,
			},
		},
		{
			// A deposit from an exchange and one from the lnd
			// wallet: only the wallet deposit's fee is known.
			name:  "wallet and externally funded deposits",
			state: Succeeded,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopedIn, known(1_550)),
				costDeposit(deposit.LoopedIn, unknown),
			},
			cost: OnchainCost{
				Amount: 1_550, Status: OnchainCostPartial,
			},
		},
		{
			name:  "externally funded and wallet deposits",
			state: Succeeded,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopedIn, unknown),
				costDeposit(deposit.LoopedIn, known(1_550)),
			},
			cost: OnchainCost{
				Amount: 1_550, Status: OnchainCostPartial,
			},
		},
		{
			name:  "only externally funded deposits",
			state: Succeeded,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopedIn, unknown),
			},
			cost: OnchainCost{Status: OnchainCostUnknown},
		},
		{
			name:  "cost of a deposit not determined yet",
			state: Succeeded,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopedIn, known(900)),
				costDeposit(deposit.LoopedIn, undetermined),
			},
			cost: OnchainCost{
				Amount: 900, Status: OnchainCostPending,
			},
		},
		{
			name:  "external deposit, other not determined yet",
			state: Succeeded,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopedIn, unknown),
				costDeposit(deposit.LoopedIn, undetermined),
			},
			cost: OnchainCost{Status: OnchainCostPending},
		},
		{
			name:  "change only",
			state: Succeeded,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopedIn, known(0)),
			},
			cost: OnchainCost{Status: OnchainCostComplete},
		},
		{
			name:  "change and an externally funded deposit",
			state: Succeeded,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopedIn, known(0)),
				costDeposit(deposit.LoopedIn, unknown),
			},
			cost: OnchainCost{Status: OnchainCostPartial},
		},
		{
			name:  "htlc timeout path swept",
			state: HtlcTimeoutSwept,
			deposits: []*deposit.Deposit{
				costDeposit(
					deposit.HtlcTimeoutSwept, known(900),
				),
			},
			cost: OnchainCost{
				Amount: 900, Status: OnchainCostComplete,
			},
		},
		{
			name:  "sweeping the htlc timeout path",
			state: SweepHtlcTimeout,
			deposits: []*deposit.Deposit{
				costDeposit(
					deposit.SweepHtlcTimeout, known(900),
				),
			},
			cost: OnchainCost{
				Amount: 900, Status: OnchainCostComplete,
			},
		},
		{
			name:  "htlc timeout sweep pending",
			state: MonitorHtlcTimeoutSweep,
			deposits: []*deposit.Deposit{
				costDeposit(
					deposit.SweepHtlcTimeout, known(900),
				),
			},
			cost: OnchainCost{
				Amount: 900, Status: OnchainCostComplete,
			},
		},
		{
			// The swap's htlc spent the deposits, and the swap
			// failed after publishing its htlc timeout sweep.
			name:    "failed after the htlc timeout sweep",
			state:   Failed,
			sweepTx: true,
			deposits: []*deposit.Deposit{
				costDeposit(
					deposit.SweepHtlcTimeout, known(900),
				),
			},
			cost: OnchainCost{
				Amount: 900, Status: OnchainCostComplete,
			},
		},
		{
			// A swap that failed after its htlc timeout sweep only
			// counts the deposits on the htlc timeout path.
			name:    "failed after the sweep, deposit off the path",
			state:   Failed,
			sweepTx: true,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopedIn, known(900)),
			},
			cost: OnchainCost{Status: OnchainCostUnknown},
		},
		{
			// A later swap spent the deposits of this failed
			// attempt through its htlc, but the deposit swap hash
			// migration mapped them to the failed attempt.
			name:  "failed, deposits spent by a later swap's htlc",
			state: Failed,
			deposits: []*deposit.Deposit{
				costDeposit(
					deposit.HtlcTimeoutSwept, known(1_550),
				),
			},
			cost: OnchainCost{Status: OnchainCostUnknown},
		},
		{
			// The swap failed before its htlc confirmed and gave
			// its deposits back.
			name:  "failed before the htlc confirmed",
			state: Failed,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.Deposited, known(900)),
			},
			cost: OnchainCost{Status: OnchainCostUnknown},
		},
		{
			// A later swap looped in the deposits of this failed
			// attempt, but the deposit swap hash migration mapped
			// them to the failed attempt.
			name:  "failed, deposits looped in by a later swap",
			state: Failed,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.LoopedIn, known(1_550)),
			},
			cost: OnchainCost{Status: OnchainCostUnknown},
		},
		{
			name:  "failed, deposits withdrawn afterwards",
			state: Failed,
			deposits: []*deposit.Deposit{
				costDeposit(deposit.Withdrawn, known(900)),
			},
			cost: OnchainCost{Status: OnchainCostUnknown},
		},
		{
			name:     "no deposits",
			state:    Succeeded,
			deposits: nil,
			cost:     OnchainCost{Status: OnchainCostUnknown},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			swp := &StaticAddressLoopIn{}
			swp.SetState(tc.state)
			if tc.sweepTx {
				swp.HtlcTimeoutSweepTxHash = &chainhash.Hash{1}
			}

			require.Equal(
				t, tc.cost, SwapOnchainCost(swp, tc.deposits),
			)
		})
	}
}
