package main

import (
	"encoding/json"
	"fmt"

	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/urfave/cli"
)

// validateRouteHints ensures that the Private flag isn't set along with
// the RouteHints flag. We don't allow both options to be set as these options
// are alternatives to each other. Private autogenerates hopHints while
// RouteHints are manually passed.
func validateRouteHints(ctx *cli.Context) ([]*swapserverrpc.RouteHint, error) {
	var hints []*swapserverrpc.RouteHint

	if ctx.IsSet(routeHintsFlag.Name) {
		if ctx.IsSet(privateFlag.Name) {
			return nil, fmt.Errorf(
				"private and route_hints both set",
			)
		}

		stringHints := []byte(ctx.String(routeHintsFlag.Name))
		err := json.Unmarshal(stringHints, &hints)
		if err != nil {
			return nil, fmt.Errorf("unable to parse json: %v", err)
		}
	}

	return hints, nil
}
