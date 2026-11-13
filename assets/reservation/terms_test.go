package reservation

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTerms(t *testing.T) {
	terms := Terms{
		AssetID:               [32]byte{1},
		Amount:                10000,
		Fee:                   10,
		CSVDelay:              1440,
		RequiredConfirmations: 3,
		ExecutionDelta:        90,
	}
	require.NoError(t, terms.Validate())

	tests := []struct {
		name   string
		change func(*Terms)
	}{
		{
			"missing asset",
			func(t *Terms) { t.AssetID = [32]byte{} },
		},
		{
			"zero amount",
			func(t *Terms) { t.Amount = 0 },
		},
		{
			"zero fee",
			func(t *Terms) { t.Fee = 0 },
		},
		{
			"amount out of range",
			func(t *Terms) { t.Amount = math.MaxUint64 },
		},
		{
			"fee out of range",
			func(t *Terms) { t.Fee = math.MaxUint64 },
		},
		{
			"gross amount overflow",
			func(t *Terms) { t.Amount = math.MaxInt64 },
		},
		{
			"zero CSV",
			func(t *Terms) { t.CSVDelay = 0 },
		},
		{
			"time CSV",
			func(t *Terms) { t.CSVDelay |= 1 << 22 },
		},
		{
			"disabled CSV",
			func(t *Terms) { t.CSVDelay |= 1 << 31 },
		},
		{
			"no confirmations",
			func(t *Terms) { t.RequiredConfirmations = 0 },
		},
		{
			"no cutoff",
			func(t *Terms) { t.ExecutionDelta = 0 },
		},
		{
			"cutoff before confirmation depth",
			func(t *Terms) { t.ExecutionDelta = 1438 },
		},
		{
			"overflow",
			func(t *Terms) { t.ExecutionDelta = math.MaxUint32 },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := terms
			test.change(&invalid)
			require.Error(t, invalid.Validate())
		})
	}

	// At three confirmations, equality leaves one block before the cutoff.
	// A larger execution margin would close execution before delivery.
	terms.ExecutionDelta = terms.CSVDelay - terms.RequiredConfirmations
	require.NoError(t, terms.Validate())
	terms.ExecutionDelta++
	require.Error(t, terms.Validate())
	terms.ExecutionDelta = 90

	// Structural validation accepts quoted fees above and below the
	// initial server price. The client must still approve those fees.
	for _, fee := range []uint64{1, 9, 10, 11, 10000} {
		quoted := terms
		quoted.Fee = fee
		require.NoError(t, quoted.Validate())
		require.Equal(t, fee, quoted.Fee)
	}

	// Check the signed-storage boundary without percentage arithmetic.
	terms.Fee = math.MaxInt64 - terms.Amount
	require.NoError(t, terms.Validate())
	terms.Fee++
	require.Error(t, terms.Validate())
}
