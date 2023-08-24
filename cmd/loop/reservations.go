package main

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var reservationsCommands = cli.Command{

	Name:      "reservations",
	ShortName: "r",
	Usage:     "manage reservations",
	Description: `
		With loopd running, you can use this command to manage your
		reservations. Reservations are 2-of-2 multisig utxos that
		the loop server can open to clients. The reservations are used
		to enable instant swaps.
	`,
	Subcommands: []cli.Command{
		listReservationsCommand,
	},
}

var (
	listReservationsCommand = cli.Command{
		Name:      "list",
		ShortName: "l",
		Usage:     "list all reservations",
		ArgsUsage: "",
		Description: `
		List all reservations.
	`,
		Action: listReservations,
	}
)

func listReservations(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListReservations(
		context.Background(), &looprpc.ListReservationsRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
