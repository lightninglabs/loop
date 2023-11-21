package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var instantOutCommand = cli.Command{
	Name:  "instantout",
	Usage: "perform an instant off-chain to on-chain swap (looping out)",
	Description: `
	Attempts to instantly loop out into the backing lnd's wallet. The amount
	will be chosen via the cli.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "channel",
			Usage: "the comma-separated list of short " +
				"channel IDs of the channels to loop out",
		},
	},
	Action: instantOut,
}

func instantOut(ctx *cli.Context) error {
	// Parse outgoing channel set. Don't string split if the flag is empty.
	// Otherwise, strings.Split returns a slice of length one with an empty
	// element.
	var outgoingChanSet []uint64
	if ctx.IsSet("channel") {
		chanStrings := strings.Split(ctx.String("channel"), ",")
		for _, chanString := range chanStrings {
			chanID, err := strconv.ParseUint(chanString, 10, 64)
			if err != nil {
				return fmt.Errorf("error parsing channel id "+
					"\"%v\"", chanString)
			}
			outgoingChanSet = append(outgoingChanSet, chanID)
		}
	}

	// First set up the swap client itself.
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// Now we fetch all the confirmed reservations.
	reservations, err := client.ListReservations(
		context.Background(), &looprpc.ListReservationsRequest{},
	)
	if err != nil {
		return err
	}

	var (
		confirmedReservations []*looprpc.ClientReservation
		totalAmt              int64
		idx                   int
	)

	fmt.Printf("Available reservations: \n\n")
	for _, res := range reservations.Reservations {
		if res.State != string(reservation.Confirmed) {
			continue
		}

		idx++
		confirmedReservations = append(confirmedReservations, res)
		fmt.Printf("Reservation %v: %v \n", idx, res.Amount)
		totalAmt += int64(res.Amount)
	}

	fmt.Println()
	fmt.Printf("Max amount to instant out: %v\n", totalAmt)
	fmt.Println()

	fmt.Printf("Select reservations for instantout (e.g. '1,2,3') \n")
	fmt.Printf("Type 'ALL' to use all available reservations. \n")

	var answer string
	fmt.Scanln(&answer)

	// Parse
	var selectedReservations [][]byte
	if answer == "ALL" {
		for _, res := range confirmedReservations {
			selectedReservations = append(
				selectedReservations,
				res.ReservationId,
			)
		}
	} else {
		selectedIndexes := strings.Split(answer, ",")
		for _, idxStr := range selectedIndexes {
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				return err
			}

			selectedReservations = append(
				selectedReservations,
				confirmedReservations[idx-1].ReservationId,
			)
		}
	}

	fmt.Printf("Starting instant swap out \n")

	// Now we can request the instant out swap.
	instantOutRes, err := client.InstantOut(
		context.Background(),
		&looprpc.InstantOutRequest{
			ReservationIds:  selectedReservations,
			OutgoingChanSet: outgoingChanSet,
		},
	)

	if err != nil {
		return err
	}

	fmt.Printf("Instant out swap initiated with ID: %x, State: %v \n",
		instantOutRes.InstantOutHash, instantOutRes.State)

	if instantOutRes.SweepTxId != "" {
		fmt.Printf("Sweepless sweep tx id: %v \n",
			instantOutRes.SweepTxId)
	}

	return nil
}
