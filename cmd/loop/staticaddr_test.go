package main

import (
	"context"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestStaticAddressDepositRequestAllowsNoUtxos(t *testing.T) {
	t.Parallel()

	var req *looprpc.NewStaticAddressRequest
	cmd := &cli.Command{
		Name:  "deposit",
		Flags: depositStaticAddressCommand.Flags,
		Action: func(_ context.Context, cmd *cli.Command) error {
			var err error
			req, err = staticAddressDepositRequest(
				cmd, "bcrt1ptestaddress",
			)

			return err
		},
	}

	err := cmd.Run(context.Background(), []string{
		"deposit", "--amt", "1000000",
	})
	require.NoError(t, err)
	require.Equal(t, "bcrt1ptestaddress", req.GetSendCoinsRequest().Addr)
	require.EqualValues(t, 1_000_000, req.GetSendCoinsRequest().Amount)
	require.Empty(t, req.GetSendCoinsRequest().Outpoints)
}

// TestLowConfDepositWarningConfirmedOnly verifies confirmed deposits below the
// conservative warning threshold are included in the warning text.
func TestLowConfDepositWarningConfirmedOnly(t *testing.T) {
	t.Parallel()

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "confirmed-low",
			ConfirmationHeight: 100,
			BlocksUntilExpiry:  140,
		},
		{
			Outpoint:           "confirmed-high",
			ConfirmationHeight: 95,
			BlocksUntilExpiry:  139,
		},
	}

	warning := lowConfDepositWarning(
		deposits, []string{"confirmed-low", "confirmed-high"}, 144,
	)

	require.Contains(t, warning, "confirmed-low (5 confirmations)")
	require.NotContains(t, warning, "confirmed-high")
}

// TestLowConfDepositWarningUnconfirmed verifies unconfirmed deposits get a
// warning that the swap may wait for confirmation-risk acceptance.
func TestLowConfDepositWarningUnconfirmed(t *testing.T) {
	t.Parallel()

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "mempool",
			ConfirmationHeight: 0,
			BlocksUntilExpiry:  144,
		},
	}

	warning := lowConfDepositWarning(deposits, []string{"mempool"}, 144)

	require.Contains(t, warning, "mempool (unconfirmed)")
	require.True(
		t,
		strings.Contains(
			warning,
			"conservative 6-confirmation threshold",
		),
	)
	require.NotContains(t, warning, "executed immediately")
}

// TestWarningDepositOutpointsAutoSelectPrefersConfirmed verifies automatic
// warning selection keeps the loop-in preference for confirmed outputs.
func TestWarningDepositOutpointsAutoSelectPrefersConfirmed(t *testing.T) {
	t.Parallel()

	const csvExpiry = 1100

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "mempool-large",
			Value:              2_000_000,
			ConfirmationHeight: 0,
			BlocksUntilExpiry:  csvExpiry,
		},
		{
			Outpoint:           "confirmed",
			Value:              1_500_000,
			ConfirmationHeight: 100,
			BlocksUntilExpiry:  csvExpiry - 5,
		},
	}

	selected := warningDepositOutpoints(deposits, nil, true, 1_000_000)

	require.Equal(t, []string{"confirmed"}, selected)
	require.Empty(t, lowConfDepositWarning(deposits, selected, csvExpiry))
}

// TestWarningDepositOutpointsAutoSelectIncludesNeededUnconfirmed verifies the
// warning path includes mempool deposits when they are needed for the target.
func TestWarningDepositOutpointsAutoSelectIncludesNeededUnconfirmed(t *testing.T) {
	t.Parallel()

	const csvExpiry = 1100

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "confirmed-small",
			Value:              500_000,
			ConfirmationHeight: 100,
			BlocksUntilExpiry:  csvExpiry - 5,
		},
		{
			Outpoint:           "mempool-large",
			Value:              2_000_000,
			ConfirmationHeight: 0,
			BlocksUntilExpiry:  csvExpiry,
		},
	}

	selected := warningDepositOutpoints(deposits, nil, true, 1_000_000)

	require.Equal(
		t, []string{"confirmed-small", "mempool-large"}, selected,
	)

	warning := lowConfDepositWarning(deposits, selected, csvExpiry)
	require.Contains(t, warning, "mempool-large (unconfirmed)")
	require.NotContains(t, warning, "confirmed-small")
}

// TestWarningDepositSelectionMatchesLoopInSelection verifies CLI warning
// selection matches the loop-in selector.
func TestWarningDepositSelectionMatchesLoopInSelection(t *testing.T) {
	t.Parallel()

	const (
		blockHeight  = uint32(10_000)
		csvExpiry    = uint32(1_200)
		targetAmount = int64(2_500_000)
	)

	type fixture struct {
		name               string
		value              int64
		confirmationHeight int64
	}

	fixtures := []fixture{
		{
			name:               "mempool-huge",
			value:              3_000_000,
			confirmationHeight: 0,
		},
		{
			name:               "confirmed-later-expiry",
			value:              2_000_000,
			confirmationHeight: 9_900,
		},
		{
			name:               "confirmed-earlier-expiry",
			value:              2_000_000,
			confirmationHeight: 9_890,
		},
		{
			name:               "confirmed-small",
			value:              600_000,
			confirmationHeight: 9_900,
		},
		{
			name:               "confirmed-too-close-to-expiry",
			value:              5_000_000,
			confirmationHeight: 9_849,
		},
	}

	rpcDeposits := make([]*looprpc.Deposit, 0, len(fixtures))
	loopInDeposits := make([]*deposit.Deposit, 0, len(fixtures))
	for idx, fixture := range fixtures {
		hash := chainhash.Hash{byte(idx + 1)}
		outpoint := wire.OutPoint{
			Hash:  hash,
			Index: uint32(idx),
		}

		blocksUntilExpiry := int64(0)
		if fixture.confirmationHeight > 0 {
			blocksUntilExpiry = fixture.confirmationHeight +
				int64(csvExpiry) - int64(blockHeight)
		}

		rpcDeposits = append(rpcDeposits, &looprpc.Deposit{
			Outpoint:           outpoint.String(),
			Value:              fixture.value,
			ConfirmationHeight: fixture.confirmationHeight,
			BlocksUntilExpiry:  blocksUntilExpiry,
		})
		loopInDeposits = append(loopInDeposits, &deposit.Deposit{
			OutPoint:           outpoint,
			Value:              btcutil.Amount(fixture.value),
			ConfirmationHeight: fixture.confirmationHeight,
			AddressParams: &address.Parameters{
				Expiry: csvExpiry,
			},
		})
	}

	cliSelected := autoSelectedWarningOutpoints(
		rpcDeposits, targetAmount,
	)

	loopInSelected, err := loopin.SelectDeposits(
		btcutil.Amount(targetAmount), loopInDeposits, blockHeight,
	)
	require.NoError(t, err)

	loopInSelectedOutpoints := make([]string, 0, len(loopInSelected))
	for _, selected := range loopInSelected {
		loopInSelectedOutpoints = append(
			loopInSelectedOutpoints, selected.OutPoint.String(),
		)
	}

	require.Equal(t, loopInSelectedOutpoints, cliSelected)
}
