package main

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

var getInfoCommand = &cli.Command{
	Name:  "getinfo",
	Usage: "show general information about the loop daemon",
	Description: "Displays general information about the daemon like " +
		"current version, connection parameters and basic swap " +
		"information.",
	Action: getInfo,
}

func getInfo(ctx context.Context, cmd *cli.Command) error {
	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	cfg, err := client.GetInfo(ctx, &looprpc.GetInfoRequest{})
	if err != nil {
		return err
	}

	printRespJSON(cfg)

	return nil
}
