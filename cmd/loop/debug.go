//go:build dev
// +build dev

package main

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

func init() {
	// Register the debug command.
	commands = append(commands, forceAutoloopCmd)
}

var forceAutoloopCmd = cli.Command{
	Name: "forceautoloop",
	Usage: `
	Forces to trigger an autoloop step, regardless of the current internal 
	autoloop timer. THIS MUST NOT BE USED IN A PROD ENVIRONMENT.
	`,
	Action: forceAutoloop,
	Hidden: true,
}

func forceAutoloop(ctx *cli.Context) error {
	client, cleanup, err := getDebugClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	cfg, err := client.ForceAutoLoop(
		context.Background(), &looprpc.ForceAutoLoopRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(cfg)

	return nil
}

func getDebugClient(ctx *cli.Context) (looprpc.DebugClient, func(), error) {
	rpcServer := ctx.GlobalString("rpcserver")
	tlsCertPath, macaroonPath, err := extractPathArgs(ctx)
	if err != nil {
		return nil, nil, err
	}
	conn, err := getClientConn(rpcServer, tlsCertPath, macaroonPath)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { conn.Close() }

	debugClient := looprpc.NewDebugClient(conn)
	return debugClient, cleanup, nil
}
