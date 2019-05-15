package main

import (
	"context"
	"fmt"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var monitorCommand = cli.Command{
	Name:        "monitor",
	Usage:       "monitor progress of any active swaps",
	Description: "Allows the user to monitor progress of any active swaps",
	Action:      monitor,
}

func monitor(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	stream, err := client.Monitor(
		context.Background(), &looprpc.MonitorRequest{})
	if err != nil {
		return err
	}

	fmt.Printf("Note: offchain cost may report as 0 after loopd restart " +
		"during swap\n")

	for {
		swap, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv: %v", err)
		}
		logSwap(swap)
	}
}
