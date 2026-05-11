package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

var (
	reservationAmountFlag = &cli.Uint64Flag{
		Name:  "amt",
		Usage: "the amount in satoshis for the reservation",
	}
	reservationExpiryFlag = &cli.UintFlag{
		Name: "expiry",
		Usage: "the relative block height at which the reservation" +
			" expires",
	}
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
		newReservationCommand,
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

	newReservationCommand = &cli.Command{
		Name:    "new",
		Aliases: []string{"n"},
		Usage:   "create a new reservation",
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

func newReservation(ctx context.Context, cmd *cli.Command) error {
	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	rpcCtx, cancel := context.WithTimeout(ctx, defaultRpcTimeout)
	defer cancel()

	if !cmd.IsSet(reservationAmountFlag.Name) {
		return errors.New("amt flag missing")
	}

	if !cmd.IsSet(reservationExpiryFlag.Name) {
		return errors.New("expiry flag missing")
	}

	quoteReq, err := client.ReservationQuote(
		rpcCtx, &looprpc.ReservationQuoteRequest{
			Amt:    cmd.Uint64(reservationAmountFlag.Name),
			Expiry: uint32(cmd.Uint(reservationExpiryFlag.Name)),
		},
	)
	if err != nil {
		return err
	}

	fmt.Printf(satAmtFmt, "Reservation Cost: ", quoteReq.PrepayAmt)

	fmt.Printf("CONTINUE RESERVATION? (y/n): ")

	var answer string
	if _, err := fmt.Scanln(&answer); err != nil ||
		!strings.EqualFold(answer, "y") {

		return nil
	}

	reservationRes, err := client.ReservationRequest(
		rpcCtx, &looprpc.ReservationRequestRequest{
			Amt:          cmd.Uint64(reservationAmountFlag.Name),
			Expiry:       uint32(cmd.Uint(reservationExpiryFlag.Name)),
			MaxPrepayAmt: quoteReq.PrepayAmt,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(reservationRes)
	return nil
}

func listReservations(ctx context.Context, cmd *cli.Command) error {
	client, cleanup, err := getClient(cmd)
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
