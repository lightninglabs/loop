//go:build dev
// +build dev

package main

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

func init() {
	// Register the debug command.
	commands = append(commands, forceAutoloopCmd)
}

var forceAutoloopCmd = &cli.Command{
	Name: "forceautoloop",
	Usage: `
	Forces to trigger an autoloop step, regardless of the current internal 
	autoloop timer. THIS MUST NOT BE USED IN A PROD ENVIRONMENT.
	`,
	Action: forceAutoloop,
	Hidden: true,
}

func forceAutoloop(ctx context.Context, cmd *cli.Command) error {
	client, cleanup, err := getDebugClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	cfg, err := client.ForceAutoLoop(ctx, &looprpc.ForceAutoLoopRequest{})
	if err != nil {
		return err
	}

	printRespJSON(cfg)

	return nil
}

func getDebugClient(ctx context.Context, cmd *cli.Command) (looprpc.DebugClient, func(), error) {
	conn, cleanup, err := sessionTransport.Dial(cmd)
	if err != nil {
		return nil, nil, err
	}

	debugClient := looprpc.NewDebugClient(conn)
	return debugClient, cleanup, nil
}
