package main

import (
	"context"
	"errors"
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
		cli.StringFlag{
			Name: "addr",
			Usage: "the optional address that the looped out funds " +
				"should be sent to, if let blank the funds " +
				"will go to lnd's wallet",
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

	for _, res := range reservations.Reservations {
		if res.State != string(reservation.Confirmed) {
			continue
		}

		confirmedReservations = append(confirmedReservations, res)
	}

	if len(confirmedReservations) == 0 {
		fmt.Printf("No confirmed reservations found \n")
		return nil
	}

	fmt.Printf("Available reservations: \n\n")
	for _, res := range confirmedReservations {
		idx++
		fmt.Printf("Reservation %v: shortid %x, amt %v, expiry "+
			"height %v \n", idx, res.ReservationId[:3], res.Amount,
			res.Expiry)

		totalAmt += int64(res.Amount)
	}

	fmt.Println()
	fmt.Printf("Max amount to instant out: %v\n", totalAmt)
	fmt.Println()

	fmt.Println("Select reservations for instantout (e.g. '1,2,3')")
	fmt.Println("Type 'ALL' to use all available reservations.")

	var answer string
	fmt.Scanln(&answer)

	// Parse
	var (
		selectedReservations [][]byte
		selectedAmt          uint64
	)
	switch answer {
	case "ALL":
		for _, res := range confirmedReservations {
			selectedReservations = append(
				selectedReservations,
				res.ReservationId,
			)
			selectedAmt += res.Amount
		}

	case "":
		return fmt.Errorf("no reservations selected")

	default:
		selectedIndexes := strings.Split(answer, ",")
		selectedIndexMap := make(map[int]struct{})
		for _, idxStr := range selectedIndexes {
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				return err
			}
			if idx < 0 {
				return fmt.Errorf("invalid index %v", idx)
			}

			if idx > len(confirmedReservations) {
				return fmt.Errorf("invalid index %v", idx)
			}
			if _, ok := selectedIndexMap[idx]; ok {
				return fmt.Errorf("duplicate index %v", idx)
			}

			selectedReservations = append(
				selectedReservations,
				confirmedReservations[idx-1].ReservationId,
			)

			selectedIndexMap[idx] = struct{}{}
			selectedAmt += confirmedReservations[idx-1].Amount
		}
	}

	// Now that we have the selected reservations we can estimate the
	// fee-rates.
	quote, err := client.InstantOutQuote(
		context.Background(), &looprpc.InstantOutQuoteRequest{
			Amt:             selectedAmt,
			NumReservations: int32(len(selectedReservations)),
		},
	)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Printf(satAmtFmt, "Estimated on-chain fee:", quote.SweepFeeSat)
	fmt.Printf(satAmtFmt, "Service fee:", quote.ServiceFeeSat)
	fmt.Println()

	fmt.Printf("CONTINUE SWAP? (y/n): ")

	fmt.Scanln(&answer)
	if answer != "y" {
		return errors.New("swap canceled")
	}

	fmt.Println("Starting instant swap out")

	// Now we can request the instant out swap.
	instantOutRes, err := client.InstantOut(
		context.Background(),
		&looprpc.InstantOutRequest{
			ReservationIds:  selectedReservations,
			OutgoingChanSet: outgoingChanSet,
			DestAddr:        ctx.String("addr"),
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
