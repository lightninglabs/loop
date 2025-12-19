package main

import (
	"context"
	"fmt"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

var monitorCommand = &cli.Command{
	Name:        "monitor",
	Usage:       "monitor progress of any active swaps",
	Description: "Allows the user to monitor progress of any active swaps",
	Action:      monitor,
}

func monitor(ctx context.Context, cmd *cli.Command) error {
	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	stream, err := client.Monitor(
		ctx, &looprpc.MonitorRequest{})
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
