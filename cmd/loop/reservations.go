package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var (
	reservationAmountFlag = cli.Uint64Flag{
		Name:  "amt",
		Usage: "the amount in satoshis for the reservation",
	}
	reservationExpiryFlag = cli.UintFlag{
		Name: "expiry",
		Usage: "the relative block height at which the reservation" +
			" expires",
	}
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
		newReservationCommand,
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

	newReservationCommand = cli.Command{
		Name:      "new",
		ShortName: "n",
		Usage:     "create a new reservation",
		Description: `
		Create a new reservation with the given value and expiry.
	`,
		Action: newReservation,
		Flags: []cli.Flag{
			reservationAmountFlag,
			reservationExpiryFlag,
		},
	}
)

func newReservation(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	ctxt, cancel := context.WithTimeout(
		context.Background(), defaultRpcTimeout,
	)
	defer cancel()

	if !ctx.IsSet(reservationAmountFlag.Name) {
		return errors.New("amt flag missing")
	}

	if !ctx.IsSet(reservationExpiryFlag.Name) {
		return errors.New("expiry flag missing")
	}

	quoteReq, err := client.ReservationQuote(
		ctxt, &looprpc.ReservationQuoteRequest{
			Amt:    ctx.Uint64(reservationAmountFlag.Name),
			Expiry: uint32(ctx.Uint(reservationExpiryFlag.Name)),
		},
	)
	if err != nil {
		return err
	}

	fmt.Printf(satAmtFmt, "Reservation Cost: ", quoteReq.PrepayAmt)

	fmt.Printf("CONTINUE RESERVATION? (y/n): ")

	var answer string
	fmt.Scanln(&answer)
	if answer == "n" {
		return nil
	}

	reservationRes, err := client.ReservationRequest(
		ctxt, &looprpc.ReservationRequestRequest{
			Amt:          ctx.Uint64(reservationAmountFlag.Name),
			Expiry:       uint32(ctx.Uint(reservationExpiryFlag.Name)),
			MaxPrepayAmt: quoteReq.PrepayAmt,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(reservationRes)
	return nil
}

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
