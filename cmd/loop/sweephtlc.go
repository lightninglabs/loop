package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

// sweepHtlcCommand exposes HTLC success-path sweeping over loop CLI.
var sweepHtlcCommand = &cli.Command{
	Name:  "sweephtlc",
	Usage: "sweep an HTLC output using the preimage success path",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:     "outpoint",
			Usage:    "htlc outpoint to sweep (format: txid:vout)",
			Required: true,
		},
		&cli.StringFlag{
			Name:     "htlcaddr",
			Usage:    "htlc address corresponding to the outpoint",
			Required: true,
		},
		&cli.UintFlag{
			Name:     "feerate",
			Usage:    "fee rate to use in sat/vbyte",
			Required: true,
		},
		&cli.StringFlag{
			Name: "destaddr",
			Usage: "optional destination address; defaults to a " +
				"new wallet address",
		},
		&cli.StringFlag{
			Name: "preimage",
			Usage: "optional preimage hex to override stored " +
				"swap preimage",
		},
		&cli.BoolFlag{
			Name:  "publish",
			Usage: "publish the sweep transaction immediately",
			Value: false,
		},
	},
	Hidden: true,
	Action: sweepHtlc,
}

// sweepHtlc executes the SweepHtlc RPC and prints the sweep transaction hex.
func sweepHtlc(ctx context.Context, cmd *cli.Command) error {
	// Loopd connecting client.
	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	// Find the preimage if the user passed it.
	var preimage []byte
	if cmd.IsSet("preimage") {
		preimage, err = hex.DecodeString(cmd.String("preimage"))
		if err != nil {
			return fmt.Errorf("invalid preimage: %w", err)
		}
	}

	// Call SweepHtlc on loopd trying to sweep the HTLC.
	resp, err := client.SweepHtlc(ctx, &looprpc.SweepHtlcRequest{
		Outpoint:    cmd.String("outpoint"),
		DestAddress: cmd.String("destaddr"),
		HtlcAddress: cmd.String("htlcaddr"),
		SatPerVbyte: uint32(cmd.Uint("feerate")),
		Preimage:    preimage,
		Publish:     cmd.Bool("publish"),
	})
	if err != nil {
		return err
	}

	// Always display the raw sweep transaction.
	fmt.Printf("sweep_tx_hex: %x\n", resp.SweepTx)

	// Report publish status in a user-friendly way based on response.
	switch {
	case resp.GetNotRequested() != nil:
		fmt.Println("publish: not requested (pass --publish to " +
			"broadcast)")

	case resp.GetPublished() != nil:
		fmt.Println("publish: success")

	case resp.GetFailed() != nil:
		errMsg := resp.GetFailed().GetError()
		fmt.Printf("publish: failed: %s\n", errMsg)

		return fmt.Errorf("publish failed: %s", errMsg)

	default:
		fmt.Println("publish: unknown status")
	}

	// Print txid if the transaction is valid.
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(resp.SweepTx)); err == nil {
		fmt.Printf("sweep_txid: %s\n", tx.TxHash().String())
	} else {
		fmt.Printf("sweep_txid: could not decode tx: %v\n", err)
	}

	// Print the fee-rate.
	fmt.Printf("fee_sats: %d\n", resp.FeeSats)

	return nil
}
