package reservation

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

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
