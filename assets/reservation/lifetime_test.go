package reservation

import (
	"math"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/require"
)

// TestLifetimeFloor checks each bound at its edge, including quotes that pass
// every per-value bound but leave too few usable blocks.
func TestLifetimeFloor(t *testing.T) {
	floor := LifetimeFloorForChain(&chaincfg.MainNetParams)
	base := testReservation().Terms
	base.CSVDelay = MainnetMinCSVDelay
	base.RequiredConfirmations = MainnetMinRequiredConfirmations
	base.ExecutionDelta = MainnetMinExecutionDelta

	// The smallest mainnet quote that satisfies every bound.
	require.NoError(t, floor.Check(base))

	tooShort := func(change func(*Terms)) Terms {
		terms := base
		change(&terms)
		return terms
	}
	for name, terms := range map[string]Terms{
		"csv": tooShort(func(t *Terms) {
			t.CSVDelay--
		}),
		"confirmations": tooShort(func(t *Terms) {
			t.RequiredConfirmations--
		}),
		"delta": tooShort(func(t *Terms) {
			t.ExecutionDelta--
		}),
		"large delta": tooShort(func(t *Terms) {
			t.ExecutionDelta = t.CSVDelay -
				t.RequiredConfirmations - MainnetMinUsableBlocks + 1
		}),
		"large confirmations": tooShort(func(t *Terms) {
			t.RequiredConfirmations = t.CSVDelay -
				t.ExecutionDelta - MainnetMinUsableBlocks + 1
		}),
	} {
		require.Error(t, floor.Check(terms), name)
	}

	// The usable window is inclusive of its minimum.
	edge := base
	edge.ExecutionDelta = edge.CSVDelay - edge.RequiredConfirmations -
		MainnetMinUsableBlocks
	require.NoError(t, floor.Check(edge))

	// The server's default policy passes the mainnet floor.
	server := base
	server.CSVDelay, server.RequiredConfirmations = 1440, 3
	server.ExecutionDelta = 90
	require.NoError(t, floor.Check(server))

	// Other networks accept any consistent terms, but still reject
	// inconsistent ones.
	for _, params := range []*chaincfg.Params{
		&chaincfg.TestNet3Params, &chaincfg.RegressionNetParams,
		&chaincfg.SigNetParams, &chaincfg.SimNetParams, nil,
	} {
		short := base
		short.CSVDelay, short.RequiredConfirmations = 3, 1
		short.ExecutionDelta = 1
		require.NoError(t, LifetimeFloorForChain(params).Check(short))

		short.ExecutionDelta = 3
		require.Error(t, LifetimeFloorForChain(params).Check(short))
	}
}

func TestLifetime(t *testing.T) {
	terms := testReservation().Terms
	lifetime, err := terms.Lifetime(100)
	require.NoError(t, err)
	require.EqualValues(t, 1540, lifetime.TimeoutHeight)
	require.EqualValues(t, 1450, lifetime.ExecutionCutoff)

	// CSV counts from the first confirmation, not from the delivery block.
	afterRestart, err := terms.Lifetime(100)
	require.NoError(t, err)
	require.Equal(t, lifetime, afterRestart)

	for _, height := range []uint32{0, math.MaxInt32, math.MaxUint32} {
		_, err := terms.Lifetime(height)
		require.Error(t, err)
	}

	// The highest representable expiry is valid; the next one is not.
	lastConfirmation := uint32(math.MaxInt32) - terms.CSVDelay
	lifetime, err = terms.Lifetime(lastConfirmation)
	require.NoError(t, err)
	require.EqualValues(t, math.MaxInt32, lifetime.TimeoutHeight)
	_, err = terms.Lifetime(lastConfirmation + 1)
	require.Error(t, err)

	// Even caller-supplied policy values go through structural validation.
	terms.ExecutionDelta = math.MaxUint32
	_, err = terms.Lifetime(100)
	require.Error(t, err)
}
