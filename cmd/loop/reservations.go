package main

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

var reservationsCommands = &cli.Command{

	Name:    "reservations",
	Aliases: []string{"r"},
	Usage:   "manage reservations",
	Description: `
		With loopd running, you can use this command to manage your
		reservations. Reservations are 2-of-2 multisig utxos that
		the loop server can open to clients. The reservations are used
		to enable instant swaps.
	`,
	Commands: []*cli.Command{
		listReservationsCommand,
	},
}

var (
	listReservationsCommand = &cli.Command{
		Name:      "list",
		Aliases:   []string{"l"},
		Usage:     "list all reservations",
		ArgsUsage: "",
		Description: `
		List all reservations.
	`,
		Action: listReservations,
	}
)

func listReservations(ctx context.Context, cmd *cli.Command) error {
	client, cleanup, err := getClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListReservations(
		ctx, &looprpc.ListReservationsRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
