package liquidity

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestDispatchStickyLoopOutMinimum tests that the amount backoff of a sticky
// loop out stops at the minimum amount provided, rather than retrying with a
// smaller swap than the client allows.
func TestDispatchStickyLoopOutMinimum(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string

		// minAmount is the smallest swap that may be retried.
		minAmount btcutil.Amount

		// amounts are the swap amounts that we expect to be
		// dispatched, in order.
		amounts []btcutil.Amount
	}{
		{
			// Without a minimum, each failed payment is retried
			// with a quarter less, until the retries run out.
			name:    "no minimum",
			amounts: []btcutil.Amount{6000, 4500, 3375},
		},
		{
			// The second retry would be 3375, below the minimum.
			name:      "stops at the minimum",
			minAmount: 4000,
			amounts:   []btcutil.Amount{6000, 4500},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg, _ := newTestConfig()

			var (
				mu      sync.Mutex
				amounts []btcutil.Amount
			)
			cfg.LoopOut = func(_ context.Context,
				request *loop.OutRequest) (
				*loop.LoopOutSwapInfo, error) {

				mu.Lock()
				defer mu.Unlock()

				amounts = append(amounts, request.Amount)

				return &loop.LoopOutSwapInfo{}, nil
			}

			// Every payment to the server fails, so each swap that
			// is dispatched is retried with a smaller amount.
			cfg.GetLoopOut = func(context.Context, lntypes.Hash) (
				*loopdb.LoopOut, error) {

				return &loopdb.LoopOut{
					Events: []*loopdb.LoopEvent{{
						State: loopdb.StateFailOffchainPayments,
					}},
				}, nil
			}

			manager := NewManager(cfg)
			manager.params.MaxAutoInFlight = 1
			manager.params.CustomPaymentCheckInterval = time.Millisecond

			manager.dispatchStickyLoopOut(
				t.Context(), loop.OutRequest{Amount: 6000}, 3,
				testCase.minAmount,
			)

			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, testCase.amounts, amounts)
		})
	}
}
