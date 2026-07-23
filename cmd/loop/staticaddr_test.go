package main

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/stretchr/testify/require"
)

// TestSelectAllStaticLoopInDeposits_defaultMinExpiry verifies that the shared
// default floor keeps the canonical expiry threshold and skips deposits that
// are already too close to expiry.
func TestSelectAllStaticLoopInDeposits_defaultMinExpiry(t *testing.T) {
	t.Parallel()

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "at-threshold",
			ConfirmationHeight: 1,
			BlocksUntilExpiry:  int64(defaultStaticLoopInMinExpiryBlocks),
		},
		{
			Outpoint:           "below-threshold",
			ConfirmationHeight: 1,
			BlocksUntilExpiry:  int64(defaultStaticLoopInMinExpiryBlocks) - 1,
		},
	}

	selected, skipped, err := selectAllStaticLoopInDeposits(
		deposits, defaultStaticLoopInMinExpiryBlocks,
	)

	require.NoError(t, err)
	require.Equal(t, []string{"at-threshold"}, selected)
	require.Equal(t, []skippedStaticLoopInDeposit{
		{
			outpoint:        "below-threshold",
			remainingBlocks: int64(defaultStaticLoopInMinExpiryBlocks) - 1,
		},
	}, skipped)

	// Lowering the floor below the default must pull the near-expiry
	// deposit back into the selection instead of skipping it.
	loweredSelected, loweredSkipped, err := selectAllStaticLoopInDeposits(
		deposits, defaultStaticLoopInMinExpiryBlocks-1,
	)

	require.NoError(t, err)
	require.Equal(
		t, []string{"at-threshold", "below-threshold"}, loweredSelected,
	)
	require.Empty(t, loweredSkipped)
}

// TestSelectAllStaticLoopInDeposits_usesRaisedMinExpiry verifies that a higher
// user-supplied floor narrows --all selection without changing the canonical
// default.
func TestSelectAllStaticLoopInDeposits_usesRaisedMinExpiry(t *testing.T) {
	t.Parallel()

	minExpiry := defaultStaticLoopInMinExpiryBlocks + 25
	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "canonical-eligible",
			ConfirmationHeight: 1,
			BlocksUntilExpiry:  int64(defaultStaticLoopInMinExpiryBlocks),
		},
		{
			Outpoint:           "raised-eligible",
			ConfirmationHeight: 1,
			BlocksUntilExpiry:  int64(minExpiry),
		},
	}

	selected, skipped, err := selectAllStaticLoopInDeposits(
		deposits, minExpiry,
	)

	require.NoError(t, err)
	require.Equal(t, []string{"raised-eligible"}, selected)
	require.Equal(t, []skippedStaticLoopInDeposit{
		{
			outpoint:        "canonical-eligible",
			remainingBlocks: int64(defaultStaticLoopInMinExpiryBlocks),
		},
	}, skipped)
}

// TestSelectAllStaticLoopInDeposits_keepsUnconfirmed verifies that mempool
// deposits stay eligible when --all applies the expiry filter.
func TestSelectAllStaticLoopInDeposits_keepsUnconfirmed(t *testing.T) {
	t.Parallel()

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "mempool",
			ConfirmationHeight: 0,
			BlocksUntilExpiry:  1,
		},
	}

	// Use a floor far above the deposit's remaining blocks so the only
	// reason it can survive selection is the unconfirmed short-circuit.
	selected, skipped, err := selectAllStaticLoopInDeposits(
		deposits, defaultStaticLoopInMinExpiryBlocks+10_000,
	)

	require.NoError(t, err)
	require.Equal(t, []string{"mempool"}, selected)
	require.Empty(t, skipped)
}

// TestSelectAllStaticLoopInDeposits_keepsNegativeConfirmationHeight verifies
// that sentinel negative confirmation heights still behave like unconfirmed
// deposits for selection.
func TestSelectAllStaticLoopInDeposits_keepsNegativeConfirmationHeight(t *testing.T) {
	t.Parallel()

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "mempool-sentinel",
			ConfirmationHeight: -1,
			BlocksUntilExpiry:  1,
		},
	}

	// Use a floor far above the deposit's remaining blocks so the only
	// reason it can survive selection is the unconfirmed short-circuit.
	selected, skipped, err := selectAllStaticLoopInDeposits(
		deposits, defaultStaticLoopInMinExpiryBlocks+10_000,
	)

	require.NoError(t, err)
	require.Equal(t, []string{"mempool-sentinel"}, selected)
	require.Empty(t, skipped)
}

// TestSelectAllStaticLoopInDeposits_returnsErrorWhenAllSkipped verifies that
// --all reports a hard error when no deposit meets the expiry floor.
func TestSelectAllStaticLoopInDeposits_returnsErrorWhenAllSkipped(t *testing.T) {
	t.Parallel()

	deposits := []*looprpc.Deposit{
		{
			Outpoint:           "too-close",
			ConfirmationHeight: 1,
			BlocksUntilExpiry:  int64(defaultStaticLoopInMinExpiryBlocks) - 1,
		},
	}

	selected, skipped, err := selectAllStaticLoopInDeposits(
		deposits, defaultStaticLoopInMinExpiryBlocks,
	)

	require.ErrorContains(t, err, "no deposited outputs meet")
	require.Empty(t, selected)
	require.Len(t, skipped, 1)
}

// TestWriteSkippedStaticLoopInDeposits_reportsSkippedDeposits verifies that the
// skipped-deposit report stays deterministic and user-readable for review.
func TestWriteSkippedStaticLoopInDeposits_reportsSkippedDeposits(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	skipped := []skippedStaticLoopInDeposit{
		{
			outpoint:        "tx-b:1",
			remainingBlocks: int64(defaultStaticLoopInMinExpiryBlocks) - 1,
		},
		{
			outpoint:        "tx-a:0",
			remainingBlocks: int64(defaultStaticLoopInMinExpiryBlocks - 50),
		},
	}

	writeSkippedStaticLoopInDeposits(
		&output, skipped, defaultStaticLoopInMinExpiryBlocks,
	)

	require.Equal(
		t,
		"Skipping static address deposits too close to expiry:\n"+
			fmt.Sprintf(
				"- tx-a:0: %d blocks remaining, requires at least %d\n",
				defaultStaticLoopInMinExpiryBlocks-50,
				defaultStaticLoopInMinExpiryBlocks,
			)+
			fmt.Sprintf(
				"- tx-b:1: %d blocks remaining, requires at least %d\n",
				defaultStaticLoopInMinExpiryBlocks-1,
				defaultStaticLoopInMinExpiryBlocks,
			),
		output.String(),
	)
}

// TestStaticAddressLoopInMinExpiryBlocksAcceptsBelowDefaultWithAll verifies that
// informed --all users can choose an expiry floor below the default.
func TestStaticAddressLoopInMinExpiryBlocksAcceptsBelowDefaultWithAll(t *testing.T) {
	t.Parallel()

	minExpiry := defaultStaticLoopInMinExpiryBlocks - 1

	actualMinExpiry, err := staticAddressLoopInMinExpiryBlocks(
		true, true, minExpiry,
	)

	require.NoError(t, err)
	require.Equal(t, minExpiry, actualMinExpiry)
}

// TestWriteStaticLoopInMinExpiryWarningReportsSafetyTradeoff verifies that the
// below-default warning includes the configured value, default, and risk marker.
func TestWriteStaticLoopInMinExpiryWarningReportsSafetyTradeoff(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	minExpiry := defaultStaticLoopInMinExpiryBlocks - 1

	writeStaticLoopInMinExpiryWarning(&output, minExpiry)

	warning := output.String()
	require.Contains(t, warning, fmt.Sprintf("set to %d", minExpiry))
	require.Contains(t, warning, fmt.Sprintf(
		"safety minimum of %d", defaultStaticLoopInMinExpiryBlocks,
	))
	require.Contains(t, warning, "WARNING")
	require.Contains(t, warning, "risk")
}

// TestStaticAddressLoopInMinExpiryBlocksRejectsTooLargeValue verifies that the
// uint64-to-int64 boundary check is preserved before deposit comparisons.
func TestStaticAddressLoopInMinExpiryBlocksRejectsTooLargeValue(t *testing.T) {
	t.Parallel()

	minExpiry := uint64(math.MaxInt64) + 1

	_, err := staticAddressLoopInMinExpiryBlocks(true, true, minExpiry)

	require.ErrorContains(t, err, "cannot exceed")
}

// TestStaticAddressLoopInMinExpiryBlocksRejectsFlagWithoutAll verifies that the
// custom expiry floor remains tied to --all selection.
func TestStaticAddressLoopInMinExpiryBlocksRejectsFlagWithoutAll(t *testing.T) {
	t.Parallel()

	_, err := staticAddressLoopInMinExpiryBlocks(
		false, true, defaultStaticLoopInMinExpiryBlocks,
	)

	require.ErrorContains(t, err, "requires --all")
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
		})
	}

	cliSelected := autoSelectedWarningOutpoints(
		rpcDeposits, targetAmount,
	)

	loopInSelected, err := loopin.SelectDeposits(
		btcutil.Amount(targetAmount), loopInDeposits, csvExpiry,
		blockHeight,
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
