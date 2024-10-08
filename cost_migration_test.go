package loop

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestCalculateLoopOutCost tests the CalculateLoopOutCost function.
func TestCalculateLoopOutCost(t *testing.T) {
	// Set up test context objects.
	lnd := test.NewMockLnd()
	server := newServerMock(lnd)
	store := loopdb.NewStoreMock(t)

	cfg := &swapConfig{
		lnd:    &lnd.LndServices,
		store:  store,
		server: server,
	}

	height := int32(600)
	req := *testRequest
	initResult, err := newLoopOutSwap(
		context.Background(), cfg, height, &req,
	)
	require.NoError(t, err)
	swap, err := store.FetchLoopOutSwap(
		context.Background(), initResult.swap.hash,
	)
	require.NoError(t, err)

	// Override the chain cost so it's negative.
	const expectedChainCost = btcutil.Amount(1000)

	// Now we have the swap and prepay invoices so let's calculate the
	// costs without providing the payments first, so we don't account for
	// any routing fees.
	paymentFees := make(map[lntypes.Hash]lnwire.MilliSatoshi)
	_, err = CalculateLoopOutCost(lnd.ChainParams, swap, paymentFees)

	// We expect that the call fails as the swap isn't finished yet.
	require.Error(t, err)

	// Override the swap state to make it look like the swap is finished
	// and make the chain cost negative too, so we can test that it'll be
	// corrected to be positive in the cost calculation.
	swap.Events = append(
		swap.Events, &loopdb.LoopEvent{
			SwapStateData: loopdb.SwapStateData{
				State: loopdb.StateSuccess,
				Cost: loopdb.SwapCost{
					Onchain: -expectedChainCost,
				},
			},
		},
	)
	costs, err := CalculateLoopOutCost(lnd.ChainParams, swap, paymentFees)
	require.NoError(t, err)

	expectedServerCost := server.swapInvoiceAmt + server.prepayInvoiceAmt -
		swap.Contract.AmountRequested
	require.Equal(t, expectedServerCost, costs.Server)
	require.Equal(t, btcutil.Amount(0), costs.Offchain)
	require.Equal(t, expectedChainCost, costs.Onchain)

	// Now add the two payments to the payments map and calculate the costs
	// again. We expect that the routng fees are now accounted for.
	paymentFees[server.swapHash] = lnwire.NewMSatFromSatoshis(44)
	paymentFees[server.prepayHash] = lnwire.NewMSatFromSatoshis(11)

	costs, err = CalculateLoopOutCost(lnd.ChainParams, swap, paymentFees)
	require.NoError(t, err)

	expectedOffchainCost := btcutil.Amount(44 + 11)
	require.Equal(t, expectedServerCost, costs.Server)
	require.Equal(t, expectedOffchainCost, costs.Offchain)
	require.Equal(t, expectedChainCost, costs.Onchain)

	// Now override the last update to make the swap timed out at the HTLC
	// sweep. We expect that the chain cost won't change, and only the
	// prepay will be accounted for.
	swap.Events[0] = &loopdb.LoopEvent{
		SwapStateData: loopdb.SwapStateData{
			State: loopdb.StateFailSweepTimeout,
			Cost: loopdb.SwapCost{
				Onchain: 0,
			},
		},
	}

	costs, err = CalculateLoopOutCost(lnd.ChainParams, swap, paymentFees)
	require.NoError(t, err)

	expectedServerCost = server.prepayInvoiceAmt
	expectedOffchainCost = btcutil.Amount(11)
	require.Equal(t, expectedServerCost, costs.Server)
	require.Equal(t, expectedOffchainCost, costs.Offchain)
	require.Equal(t, btcutil.Amount(0), costs.Onchain)
}

// TestCostMigration tests the cost migration for loop out swaps.
func TestCostMigration(t *testing.T) {
	// Set up test context objects.
	lnd := test.NewMockLnd()
	server := newServerMock(lnd)
	store := loopdb.NewStoreMock(t)

	cfg := &swapConfig{
		lnd:    &lnd.LndServices,
		store:  store,
		server: server,
	}

	height := int32(600)
	req := *testRequest
	initResult, err := newLoopOutSwap(
		context.Background(), cfg, height, &req,
	)
	require.NoError(t, err)

	// Override the chain cost so it's negative.
	const expectedChainCost = btcutil.Amount(1000)

	// Override the swap state to make it look like the swap is finished
	// and make the chain cost negative too, so we can test that it'll be
	// corrected to be positive in the cost calculation.
	err = store.UpdateLoopOut(
		context.Background(), initResult.swap.hash, time.Now(),
		loopdb.SwapStateData{
			State: loopdb.StateSuccess,
			Cost: loopdb.SwapCost{
				Onchain: -expectedChainCost,
			},
		},
	)
	require.NoError(t, err)

	// Add the two mocked payment to LND. Note that we only care about the
	// fees here, so we don't need to provide the full payment details.
	lnd.Payments = []lndclient.Payment{
		{
			Hash: server.swapHash,
			Fee:  lnwire.NewMSatFromSatoshis(44),
		},
		{
			Hash: server.prepayHash,
			Fee:  lnwire.NewMSatFromSatoshis(11),
		},
	}

	// Now we can run the migration.
	err = MigrateLoopOutCosts(context.Background(), lnd.LndServices, 1, store)
	require.NoError(t, err)

	// Finally check that the swap cost has been updated correctly.
	swap, err := store.FetchLoopOutSwap(
		context.Background(), initResult.swap.hash,
	)
	require.NoError(t, err)

	expectedServerCost := server.swapInvoiceAmt + server.prepayInvoiceAmt -
		swap.Contract.AmountRequested

	costs := swap.Events[0].Cost
	expectedOffchainCost := btcutil.Amount(44 + 11)
	require.Equal(t, expectedServerCost, costs.Server)
	require.Equal(t, expectedOffchainCost, costs.Offchain)
	require.Equal(t, expectedChainCost, costs.Onchain)

	// Now run the migration again to make sure it doesn't fail. This also
	// indicates that the migration did not run the second time as
	// otherwise the store mocks SetMigration function would fail.
	err = MigrateLoopOutCosts(context.Background(), lnd.LndServices, 1, store)
	require.NoError(t, err)
}
