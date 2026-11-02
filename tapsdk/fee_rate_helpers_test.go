package tapsdk

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func mustFeeRateSatPerVByte(t *testing.T, satPerVByte uint64) FeeRate {
	t.Helper()

	feeRate, err := NewFeeRateSatPerVByte(satPerVByte)
	require.NoError(t, err)

	return feeRate
}
