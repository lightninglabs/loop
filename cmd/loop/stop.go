package main

import (
	"context"
	"fmt"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

var stopCommand = &cli.Command{
	Name:        "stop",
	Usage:       "stop the loop daemon",
	Description: "Requests loopd to perform a graceful shutdown.",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "wait",
			Usage: "wait until loopd fully shuts down",
		},
	},
	Action: stopDaemon,
}

// stopDaemon requests the daemon to shut down gracefully and optionally waits
// for the gRPC connection to terminate.
func stopDaemon(ctx context.Context, cmd *cli.Command) error {
	waitForShutdown := cmd.Bool("wait")

	// Establish a client connection to loopd.
	client, conn, cleanup, err := getClientWithConn(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	// Request loopd to shut down.
	_, err = client.StopDaemon(ctx, &looprpc.StopDaemonRequest{})
	if err != nil {
		return err
	}

	fmt.Println("Shutting down loopd")

	// Optionally wait for the gRPC connection to terminate.
	if !waitForShutdown {
		return nil
	}

	fmt.Println("Waiting for loopd to exit...")

	err = waitForDaemonShutdown(ctx, conn)
	if err != nil {
		return err
	}

	fmt.Println("Loopd shut down")

	return nil
}

// waitForDaemonShutdown monitors the gRPC connectivity state until the daemon
// disappears. To avoid getting stuck in idle mode we nudge the connection to
// reconnect when needed.
func waitForDaemonShutdown(ctx context.Context, conn *grpc.ClientConn) error {
	for {
		state := conn.GetState()

		switch state {
		case connectivity.Shutdown:
			return nil

		// Connection attempts now fail which means loopd is offline.
		case connectivity.TransientFailure:
			return nil

		// Force the channel out of Idle so we'll see failure states
		// once loopd stops serving.
		case connectivity.Idle:
			conn.Connect()
		}

		if !conn.WaitForStateChange(ctx, state) {
			return ctx.Err()
		}
	}
}
