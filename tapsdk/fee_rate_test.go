package tapsdk

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewFeeRateSatPerVByte(t *testing.T) {
	t.Parallel()

	rate, err := NewFeeRateSatPerVByte(2)
	require.NoError(t, err)
	require.Equal(t, uint64(2000), rate.SatPerKVByte())
	require.Equal(t, uint64(500), rate.SatPerKWeight())
	require.Equal(t, uint64(2), rate.SatPerVByteFloor())
	require.Equal(t, uint64(2), rate.SatPerVByteCeil())
	require.False(t, rate.IsZero())

	_, err = NewFeeRateSatPerVByte(
		math.MaxUint64/feeRateVBytesPerKVByte + 1,
	)
	require.ErrorIs(t, err, ErrInvalidFeeRate)
}

func TestParseFeeRateSatPerVByte(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		value         string
		wantSatPerKVB uint64
		wantSatPerKW  uint64
		wantString    string
	}{
		{
			name:          "zero",
			value:         "0",
			wantSatPerKVB: 0,
			wantSatPerKW:  0,
			wantString:    "0 sat/vB",
		},
		{
			name:          "whole",
			value:         "2",
			wantSatPerKVB: 2000,
			wantSatPerKW:  500,
			wantString:    "2 sat/vB",
		},
		{
			name:          "single decimal",
			value:         "1.1",
			wantSatPerKVB: 1100,
			wantSatPerKW:  275,
			wantString:    "1.1 sat/vB",
		},
		{
			name:          "three decimals",
			value:         "1.234",
			wantSatPerKVB: 1234,
			wantSatPerKW:  309,
			wantString:    "1.234 sat/vB",
		},
		{
			name:          "leading decimal",
			value:         ".25",
			wantSatPerKVB: 250,
			wantSatPerKW:  63,
			wantString:    "0.25 sat/vB",
		},
		{
			name:          "trailing decimal",
			value:         "3.",
			wantSatPerKVB: 3000,
			wantSatPerKW:  750,
			wantString:    "3 sat/vB",
		},
		{
			name:          "trim spaces",
			value:         " 4.5 ",
			wantSatPerKVB: 4500,
			wantSatPerKW:  1125,
			wantString:    "4.5 sat/vB",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rate, err := ParseFeeRateSatPerVByte(tc.value)
			require.NoError(t, err)
			require.Equal(t, tc.wantSatPerKVB, rate.SatPerKVByte())
			require.Equal(t, tc.wantSatPerKW, rate.SatPerKWeight())
			require.Equal(t, tc.wantString, rate.String())
		})
	}
}

func TestParseFeeRateSatPerVByteErrors(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"-1",
		testAbc,
		"1.a",
		"1.1234",
		"1.2.3",
	}

	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			_, err := ParseFeeRateSatPerVByte(value)
			require.ErrorIs(t, err, ErrInvalidFeeRate)
		})
	}
}

func TestFeeRateSatPerVByteRounding(t *testing.T) {
	t.Parallel()

	rate := NewFeeRateSatPerKVByte(1001)
	require.Equal(t, uint64(1), rate.SatPerVByteFloor())
	require.Equal(t, uint64(2), rate.SatPerVByteCeil())
	require.Equal(t, uint64(251), rate.SatPerKWeight())
}

func TestFeeRateTextEncoding(t *testing.T) {
	t.Parallel()

	rate, err := ParseFeeRateSatPerVByte("1.1")
	require.NoError(t, err)

	text, err := rate.MarshalText()
	require.NoError(t, err)
	require.Equal(t, "1.1", string(text))

	var decoded FeeRate
	require.NoError(t, decoded.UnmarshalText(text))
	require.Equal(t, rate, decoded)
}

func TestFeeRateJSONEncoding(t *testing.T) {
	t.Parallel()

	rate, err := ParseFeeRateSatPerVByte("1.1")
	require.NoError(t, err)

	data, err := json.Marshal(rate)
	require.NoError(t, err)
	require.Equal(t, `"1.1"`, string(data))

	var decoded FeeRate
	require.NoError(t, json.Unmarshal([]byte(`"1.1"`), &decoded))
	require.Equal(t, rate, decoded)
}
