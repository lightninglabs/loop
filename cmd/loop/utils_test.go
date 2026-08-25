package main

import (
	"context"
	"testing"

	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

// TestRouteHintsCLIParsing verifies that the real commands preserve repeated
// JSON route hints containing commas and return the decoded values.
func TestRouteHintsCLIParsing(t *testing.T) {
	const (
		firstHint = `{"hop_hints":[{"node_id":"node-1",` +
			`"chan_id":1,"fee_base_msat":1000,` +
			`"fee_proportional_millionths":1,` +
			`"cltv_expiry_delta":80}]}`
		secondHint = `{"hopHints":[{"nodeId":"node-2",` +
			`"chanId":2,"feeBaseMsat":2000,` +
			`"feeProportionalMillionths":2,` +
			`"cltvExpiryDelta":81}]}`
	)

	testCases := []struct {
		name      string
		path      []string
		sliceFlag string
	}{
		{
			name: "loop in",
			path: []string{"in"},
		},
		{
			name:      "quote in",
			path:      []string{"quote", "in"},
			sliceFlag: "deposit_outpoint",
		},
		{
			name:      "static in",
			path:      []string{"static", "in"},
			sliceFlag: "utxo",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			rootCommand := newRootCommandForReplay()
			routeHintCommand := commandAtPath(
				t, rootCommand, testCase.path,
			)

			var (
				routeHints  []*swapserverrpc.RouteHint
				sliceValues []string
			)
			routeHintCommand.Action = func(_ context.Context,
				cmd *cli.Command) error {

				if testCase.sliceFlag != "" {
					sliceValues = commaSeparatedStringSlice(
						cmd,
						testCase.sliceFlag,
					)
				}

				var err error
				routeHints, err = validateRouteHints(cmd)

				return err
			}

			args := append([]string{"loop"}, testCase.path...)
			args = append(
				args, "--route_hints", firstHint,
				"--route_hints", secondHint,
			)
			if testCase.sliceFlag != "" {
				args = append(
					args, "--"+testCase.sliceFlag,
					"first:0,second:1",
				)
			}

			err := rootCommand.Run(t.Context(), args)
			require.NoError(t, err)
			require.Len(t, routeHints, 2)
			if testCase.sliceFlag != "" {
				require.Equal(
					t, []string{"first:0", "second:1"},
					sliceValues,
				)
			}

			require.Len(t, routeHints[0].HopHints, 1)
			require.Equal(
				t, "node-1", routeHints[0].HopHints[0].NodeId,
			)
			require.Equal(
				t, uint64(1), routeHints[0].HopHints[0].ChanId,
			)
			require.Equal(
				t, uint32(1000),
				routeHints[0].HopHints[0].FeeBaseMsat,
			)
			require.Equal(
				t, uint32(1), routeHints[0].HopHints[0].
					FeeProportionalMillionths,
			)
			require.Equal(
				t, uint32(80),
				routeHints[0].HopHints[0].CltvExpiryDelta,
			)

			require.Len(t, routeHints[1].HopHints, 1)
			require.Equal(
				t, "node-2", routeHints[1].HopHints[0].NodeId,
			)
			require.Equal(
				t, uint64(2), routeHints[1].HopHints[0].ChanId,
			)
		})
	}
}

// TestRouteHintsRejectUnknownFields verifies that misspelled protobuf fields
// fail at the CLI boundary instead of producing zero-valued route hints.
func TestRouteHintsRejectUnknownFields(t *testing.T) {
	rootCommand := newRootCommandForReplay()
	routeHintCommand := commandAtPath(t, rootCommand, []string{"in"})
	routeHintCommand.Action = func(_ context.Context,
		cmd *cli.Command) error {

		_, err := validateRouteHints(cmd)

		return err
	}

	err := rootCommand.Run(t.Context(), []string{
		"loop", "in", "--route_hints",
		`{"hop_hints":[{"node_id":"node-1","chan_id":1,` +
			`"cltv_expiry_delat":80}]}`,
	})
	require.ErrorContains(t, err, "unknown field")
}

// commandAtPath returns a command from root by following path.
func commandAtPath(t *testing.T, root *cli.Command,
	path []string) *cli.Command {

	t.Helper()

	command := root
	for _, name := range path {
		command = command.Command(name)
		require.NotNil(t, command)
	}

	return command
}
