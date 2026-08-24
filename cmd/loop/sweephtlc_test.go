package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

// TestSweepHtlcInt32Flag verifies that CLI values cannot be truncated when
// they are encoded in the RPC request.
func TestSweepHtlcInt32Flag(t *testing.T) {
	testCases := []struct {
		name      string
		value     string
		expected  int32
		expectErr bool
	}{
		{
			name:     "maximum",
			value:    "2147483647",
			expected: 2147483647,
		},
		{
			name:      "positive overflow",
			value:     "2147483648",
			expectErr: true,
		},
		{
			name:      "negative overflow",
			value:     "-2147483649",
			expectErr: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var actual int32
			command := &cli.Command{
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "height"},
				},
				Action: func(_ context.Context,
					cmd *cli.Command) error {

					var err error
					actual, err = sweepHtlcInt32Flag(
						cmd, "height",
					)

					return err
				},
			}

			args := []string{
				"test", "--height", testCase.value,
			}
			err := command.Run(
				t.Context(), args,
			)
			if testCase.expectErr {
				require.ErrorContains(
					t, err, "outside the int32 range",
				)

				return
			}

			require.NoError(t, err)
			require.Equal(t, testCase.expected, actual)
		})
	}
}

// TestSweepHtlcUint32Flag verifies that scan limits cannot be truncated in
// the RPC request.
func TestSweepHtlcUint32Flag(t *testing.T) {
	testCases := []struct {
		name      string
		value     string
		expected  uint32
		expectErr bool
	}{
		{
			name:     "maximum",
			value:    "4294967295",
			expected: 4294967295,
		},
		{
			name:      "overflow",
			value:     "4294967296",
			expectErr: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var actual uint32
			command := &cli.Command{
				Flags: []cli.Flag{
					&cli.UintFlag{Name: "limit"},
				},
				Action: func(_ context.Context,
					cmd *cli.Command) error {

					var err error
					actual, err = sweepHtlcUint32Flag(
						cmd, "limit",
					)

					return err
				},
			}

			args := []string{
				"test", "--limit", testCase.value,
			}
			err := command.Run(
				t.Context(), args,
			)
			if testCase.expectErr {
				require.ErrorContains(
					t, err, "outside the uint32 range",
				)

				return
			}

			require.NoError(t, err)
			require.Equal(t, testCase.expected, actual)
		})
	}
}
