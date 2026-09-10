package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/assets/reservation"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

var assetCommand = &cli.Command{
	Name:  "asset",
	Usage: "manage on-chain asset reservations",
	Commands: []*cli.Command{{
		Name:  "reservation",
		Usage: "buy and inspect asset reservations",
		Commands: []*cli.Command{
			{
				Name:  "buy",
				Usage: "quote, approve, and buy a reservation",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "asset_id",
						Required: true,
					},
					&cli.Uint64Flag{
						Name:     "amt",
						Required: true,
						Usage:    "asset amount in indivisible units",
					},
					&cli.StringFlag{
						Name:  "reservation_id",
						Usage: "resume this unpaid purchase ID",
					},
					&cli.BoolFlag{
						Name:  "yes",
						Usage: "approve the displayed terms without a prompt",
					},
					&cli.BoolFlag{
						Name:  "skip_probe",
						Usage: "skip probing and buy without a successful main-payment probe",
					},
					&cli.Uint64Flag{
						Name:  "max_routing_fee",
						Usage: "maximum prepay routing fee in satoshis",
					},
				},
				Action: buyAssetReservation,
			},
			{
				Name:   "list",
				Usage:  "list saved purchases",
				Action: listAssetReservations,
			},
			{
				Name:      "get",
				Usage:     "show a reservation by outpoint",
				ArgsUsage: "txid:vout",
				Flags:     pendingReservationFlag(),
				Action:    inspectAssetReservation,
			},
			{
				Name:      "cancel",
				Usage:     "cancel an unpaid reservation",
				ArgsUsage: "txid:vout",
				Flags:     pendingReservationFlag(),
				Action:    inspectAssetReservation,
			},
			{
				Name:      "probe",
				Usage:     "retry the main-payment probe for the unpaid quote",
				ArgsUsage: "txid:vout",
				Flags:     pendingReservationFlag(),
				Action:    inspectAssetReservation,
			},
		},
	}},
}

func pendingReservationFlag() []cli.Flag {
	return []cli.Flag{&cli.StringFlag{
		Name:  "reservation_id",
		Usage: "identify a purchase before funding",
	}}
}

func reservationID(value string) ([]byte, error) {
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != value ||
		[32]byte(raw) == ([32]byte{}) {

		return nil, errors.New("expected a nonzero lowercase 32-byte hex ID")
	}
	return raw, nil
}

func localReservationSelector(cmd *cli.Command) (
	*looprpc.ClientAssetReservationSelector, error) {

	if cmd.IsSet("reservation_id") && cmd.NArg() == 0 {
		id, err := reservationID(cmd.String("reservation_id"))
		if err != nil {
			return nil, err
		}
		return reservationSelectorID(id), nil
	}
	if cmd.IsSet("reservation_id") || cmd.NArg() != 1 {
		return nil, errors.New("supply one outpoint or a pre-funding ID")
	}
	point, err := reservation.ParseOutpoint(cmd.Args().First())
	if err != nil {
		return nil, err
	}
	return &looprpc.ClientAssetReservationSelector{
		Selector: &looprpc.ClientAssetReservationSelector_Outpoint{
			Outpoint: point.String(),
		},
	}, nil
}

func reservationSelectorID(id []byte) *looprpc.ClientAssetReservationSelector {
	return &looprpc.ClientAssetReservationSelector{
		Selector: &looprpc.ClientAssetReservationSelector_ReservationId{
			ReservationId: id,
		},
	}
}

func buyAssetReservation(ctx context.Context, cmd *cli.Command) error {
	assetID, err := reservationID(cmd.String("asset_id"))
	if err != nil {
		return err
	}
	if cmd.Uint64("amt") == 0 || cmd.Uint64("amt") > math.MaxInt64 {
		return errors.New("asset amount is out of range")
	}
	id := make([]byte, 32)
	if cmd.IsSet("reservation_id") {
		id, err = reservationID(cmd.String("reservation_id"))
	} else {
		_, err = rand.Read(id)
	}
	if err != nil {
		return err
	}
	_, conn, closeConn, err := getClientWithConn(cmd)
	if err != nil {
		return err
	}
	defer closeConn()
	client := looprpc.NewAssetReservationsClient(conn)
	// Print before dispatch so a lost reply does not lose the retry key.
	fmt.Printf("Purchase ID (until funded): %x\n", id)
	r, err := client.Buy(ctx, &looprpc.BuyAssetReservationRequest{
		ReservationId: id,
		AssetId:       assetID,
		Amount:        cmd.Uint64("amt"),
		SkipProbe:     cmd.Bool("skip_probe"),
	})
	if err != nil {
		return err
	}
	selector := reservationSelectorID(id)
	r, err = waitAssetReservation(ctx, client, selector, r, true)
	if err != nil {
		return err
	}
	if r.State == string(reservation.AwaitApproval) {
		printRespJSON(r)
		fmt.Print(reservationFeeInfo(r))
		fmt.Print(reservationProbeInfo(r))
		approval, err := reservationApproval(cmd, r)
		if err != nil {
			return err
		}
		if approval.SkipProbe {
			fmt.Println("Buying without requiring a successful main-payment probe.")
		}
		fmt.Printf("Maximum prepay routing fee: %d msat\n",
			approval.MaxRouteFeeMsat)
		fmt.Println("The main BTC price is an estimate, not a rate lock.")
		fmt.Println("Prepay credits the later swap fee; unused reservations " +
			"do not refund it.")
		if !cmd.Bool("yes") {
			fmt.Print("BUY RESERVATION? (y/n): ")
			var answer string
			if _, err := fmt.Scanln(&answer); err != nil || answer != "y" {
				fmt.Println("Not approved. The unpaid purchase remains saved.")
				return nil
			}
		}
		r, err = client.Approve(ctx, approval)
		if err != nil {
			return err
		}
	}
	fmt.Println("Waiting for confirmed, verified delivery. It is safe to " +
		"close this command; the daemon will continue.")
	r, err = waitAssetReservation(ctx, client, selector, r, false)
	if err != nil {
		return err
	}
	printRespJSON(r)
	return nil
}

// reservationFeeInfo displays the full prepaid service fee, including any
// transport minimum. Integer arithmetic avoids overflow and precision loss.
func reservationFeeInfo(r *looprpc.ClientAssetReservation) string {
	if r.Amount == 0 {
		return ""
	}
	percent := new(big.Rat).SetFrac(
		new(big.Int).Mul(new(big.Int).SetUint64(r.AssetFee),
			big.NewInt(100)),
		new(big.Int).SetUint64(r.Amount),
	)
	return fmt.Sprintf("Service fee paid upfront: %d asset units "+
		"(%s%% of principal).\n"+
		"The fee includes any minimum needed to transport the prepay.\n"+
		"Remaining swap payment: %d asset units; routing and miner "+
		"fees are separate.\n", r.AssetFee, percent.FloatString(3),
		r.Amount)
}

// reservationProbeInfo describes the main probe without promising future
// liquidity. The prepay estimate is derived; no prepay probe was sent.
func reservationProbeInfo(r *looprpc.ClientAssetReservation) string {
	switch r.MainProbe {
	case looprpc.ReservationProbeResult_RESERVATION_PROBE_SUCCEEDED:
		return fmt.Sprintf("Main-payment probe: route to destination found.\n"+
			"Main routing fee estimate: %d msat\n"+
			"Approximate prepay routing fee: %d msat "+
			"(scaled by amount; fixed hop fees ignored).\n"+
			"The probe does not reserve liquidity for the later swap.\n",
			r.MainProbeFeeMsat, r.EstimatedPrepayRouteFeeMsat)

	case looprpc.ReservationProbeResult_RESERVATION_PROBE_FAILED:
		return "Main-payment probe: no route to destination found.\n" +
			"Routing fee estimates are unavailable.\n"

	case looprpc.ReservationProbeResult_RESERVATION_PROBE_TIMED_OUT:
		return "Main-payment probe: timed out; routability is unknown.\n" +
			"Routing fee estimates are unavailable.\n"

	case looprpc.ReservationProbeResult_RESERVATION_PROBE_UNSUPPORTED:
		return "Main-payment probe: unsupported; routability is unknown.\n" +
			"Routing fee estimates are unavailable.\n"

	default:
		if r.SkipProbe {
			return "Main-payment probe: skipped by request.\n" +
				"Routing fee estimates are unavailable.\n"
		}
		return "Main-payment probe: no result available.\n" +
			"Routing fee estimates are unavailable.\n"
	}
}

func reservationApproval(cmd *cli.Command, r *looprpc.ClientAssetReservation) (
	*looprpc.ApproveAssetReservationRequest, error) {

	if r == nil || len(r.QuoteHash) != 32 || r.PrepayAmountMsat == 0 ||
		r.PrepayAmountMsat > math.MaxInt64 {

		return nil, errors.New("invalid reservation quote")
	}
	if !cmd.Bool("skip_probe") &&
		r.MainProbe != looprpc.ReservationProbeResult_RESERVATION_PROBE_SUCCEEDED {

		return nil, errors.New("main-payment probe did not succeed; " +
			"retry the probe or resume this purchase with " +
			"--reservation_id and --skip_probe")
	}
	sats := (r.PrepayAmountMsat + 999) / 1000
	if sats > math.MaxInt64/uint64(maxRoutingFeeRate) {
		return nil, errors.New("prepay exceeds routing fee calculation range")
	}
	fee := uint64(getMaxRoutingFee(btcutil.Amount(sats)))
	if cmd.IsSet("max_routing_fee") {
		fee = cmd.Uint64("max_routing_fee")
	}
	if fee > math.MaxInt64/1000 {
		return nil, errors.New("routing fee exceeds payment range")
	}
	return &looprpc.ApproveAssetReservationRequest{
		Reservation:     reservationSelectorID(r.ReservationId),
		QuoteHash:       r.QuoteHash,
		MaxRouteFeeMsat: fee * 1000,
		SkipProbe:       cmd.Bool("skip_probe"),
	}, nil
}

func waitAssetReservation(ctx context.Context,
	client looprpc.AssetReservationsClient,
	selector *looprpc.ClientAssetReservationSelector,
	r *looprpc.ClientAssetReservation, quoteOnly bool) (
	*looprpc.ClientAssetReservation, error) {

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if r == nil {
			return nil, errors.New("missing reservation response")
		}
		switch r.State {
		case string(reservation.Ready):
			return r, nil

		case string(reservation.Canceled), string(reservation.Expired),
			string(reservation.NeedAdminAttention):

			return nil, fmt.Errorf("reservation ended in %s", r.State)

		case string(reservation.AwaitApproval):
			if quoteOnly {
				return r, nil
			}

		default:
			if quoteOnly && r.State != string(reservation.RequestQuote) &&
				r.State != string(reservation.ProbeRoutes) {

				return r, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-ticker.C:
		}
		var err error
		r, err = client.Get(ctx, selector)
		if err != nil {
			return nil, err
		}
	}
}

func listAssetReservations(ctx context.Context, cmd *cli.Command) error {
	_, conn, closeConn, err := getClientWithConn(cmd)
	if err != nil {
		return err
	}
	defer closeConn()
	client := looprpc.NewAssetReservationsClient(conn)
	request := &looprpc.ListClientAssetReservationsRequest{
		Limit: 100,
	}
	result := &looprpc.ListClientAssetReservationsResponse{}
	for {
		page, err := client.List(ctx, request)
		if err != nil {
			return err
		}
		result.Reservations = append(result.Reservations, page.Reservations...)
		if len(page.NextAfterId) == 0 {
			break
		}
		request.AfterId = page.NextAfterId
	}
	printRespJSON(result)
	return nil
}

func inspectAssetReservation(ctx context.Context, cmd *cli.Command) error {
	selector, err := localReservationSelector(cmd)
	if err != nil {
		return err
	}
	_, conn, closeConn, err := getClientWithConn(cmd)
	if err != nil {
		return err
	}
	defer closeConn()
	client := looprpc.NewAssetReservationsClient(conn)
	var r *looprpc.ClientAssetReservation
	switch cmd.Name {
	case "cancel":
		r, err = client.Cancel(ctx, selector)

	case "probe":
		r, err = client.RetryProbes(ctx, selector)

	default:
		r, err = client.Get(ctx, selector)
	}
	if err != nil {
		return err
	}
	printRespJSON(r)
	return nil
}
