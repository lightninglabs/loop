package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/urfave/cli/v3"
)

// showCommandHelp prints help for the current command by delegating to the
// parent command when available. This ensures help output renders even when
// invoked from inside a subcommand's action.
func showCommandHelp(ctx context.Context, cmd *cli.Command) error {
	lineage := cmd.Lineage()
	if len(lineage) > 1 {
		parent := lineage[1]
		return cli.ShowCommandHelp(ctx, parent, cmd.Name)
	}
	return cli.ShowCommandHelp(ctx, cmd, cmd.Name)
}

// validateRouteHints ensures that the Private flag isn't set along with
// the RouteHints flag. We don't allow both options to be set as these options
// are alternatives to each other. Private autogenerates hopHints while
// RouteHints are manually passed.
func validateRouteHints(cmd *cli.Command) ([]*swapserverrpc.RouteHint, error) {
	var hints []*swapserverrpc.RouteHint

	if cmd.IsSet(routeHintsFlag.Name) {
		if cmd.IsSet(privateFlag.Name) {
			return nil, fmt.Errorf(
				"private and route_hints both set",
			)
		}

		jsonHints := cmd.StringSlice(routeHintsFlag.Name)

		hints := make([]*swapserverrpc.RouteHint, len(jsonHints))
		for i, jsonHint := range jsonHints {
			var h swapserverrpc.RouteHint
			err := json.Unmarshal([]byte(jsonHint), &h)
			if err != nil {
				return nil, fmt.Errorf("unable to parse %d-th "+
					"hint json %v: %w", i, jsonHint, err)
			}
			hints[i] = &h
		}
	}

	return hints, nil
}
