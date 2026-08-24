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

// sweepHtlcCommand exposes HTLC sweeping over loop CLI.
var sweepHtlcCommand = &cli.Command{
	Name:  "sweephtlc",
	Usage: "sweep an HTLC output using a preimage or cooperation",
	Description: "Supplying any stateless-recovery flag selects " +
		"stateless recovery mode. Both public keys, the preimage, " +
		"CLTV expiry, and swap initiation height must then be " +
		"supplied. In this mode, Loop reconstructs a protocol-11 " +
		"Loop Out HTLC without querying its swap database. It " +
		"verifies the reconstructed address against the requested " +
		"address and the actual on-chain output, then asks lnd to " +
		"sign using the client public key. If signing fails or the " +
		"signature does not verify, Loop scans the configured " +
		"number of keys in key family 99 and retries with the " +
		"recovered key locator. The cooperative option instead " +
		"requests a MuSig2 server signature and spends via the " +
		"Taproot key path. Stateless cooperative recovery also " +
		"requires the swap invoice payment address.",
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
		&cli.StringFlag{
			Name: "serverpubkey",
			Usage: "compressed server HTLC public key; enables " +
				"stateless recovery mode",
		},
		&cli.StringFlag{
			Name: "clientpubkey",
			Usage: "compressed client HTLC public key; enables " +
				"stateless recovery mode",
		},
		&cli.IntFlag{
			Name: "cltvexpiry",
			Usage: "absolute HTLC CLTV expiry for " +
				"stateless recovery",
		},
		&cli.IntFlag{
			Name: "initiationheight",
			Usage: "block height at which the swap was " +
				"initiated; required for stateless recovery",
		},
		&cli.UintFlag{
			Name: "keyscanlimit",
			Usage: "maximum family-99 keys to scan; zero uses " +
				"loopd's default",
		},
		&cli.BoolFlag{
			Name:  "cooperative",
			Usage: "request a cooperative MuSig2 key-path sweep",
		},
		&cli.StringFlag{
			Name: "paymentaddr",
			Usage: "swap invoice payment address; required for " +
				"stateless cooperative recovery",
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

	decodePubKey := func(flag string) ([]byte, error) {
		if !cmd.IsSet(flag) {
			return nil, nil
		}
		if cmd.String(flag) == "" {
			return nil, fmt.Errorf("%s cannot be empty", flag)
		}

		pubKey, err := hex.DecodeString(cmd.String(flag))
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", flag, err)
		}

		return pubKey, nil
	}

	stateless := cmd.IsSet("serverpubkey") ||
		cmd.IsSet("clientpubkey") || cmd.IsSet("cltvexpiry") ||
		cmd.IsSet("initiationheight") || cmd.IsSet("keyscanlimit")

	var recovery *looprpc.StatelessRecovery
	if stateless {
		serverPubKey, keyErr := decodePubKey("serverpubkey")
		if keyErr != nil {
			return keyErr
		}

		clientPubKey, keyErr := decodePubKey("clientpubkey")
		if keyErr != nil {
			return keyErr
		}

		cltvExpiry, flagErr := sweepHtlcInt32Flag(
			cmd, "cltvexpiry",
		)
		if flagErr != nil {
			return flagErr
		}

		initiationHeight, flagErr := sweepHtlcInt32Flag(
			cmd, "initiationheight",
		)
		if flagErr != nil {
			return flagErr
		}

		keyScanLimit, flagErr := sweepHtlcUint32Flag(
			cmd, "keyscanlimit",
		)
		if flagErr != nil {
			return flagErr
		}

		recovery = &looprpc.StatelessRecovery{
			ServerPubkey:         serverPubKey,
			ClientPubkey:         clientPubKey,
			CltvExpiry:           cltvExpiry,
			SwapInitiationHeight: initiationHeight,
			KeyScanLimit:         keyScanLimit,
		}
	}

	cooperative := cmd.Bool("cooperative")
	if cmd.IsSet("paymentaddr") && !cooperative {
		return fmt.Errorf("--paymentaddr requires --cooperative")
	}

	var cooperativeSweep *looprpc.CooperativeSweep
	if cooperative {
		var paymentAddr []byte
		if cmd.IsSet("paymentaddr") {
			paymentAddr, err = hex.DecodeString(cmd.String("paymentaddr"))
			if err != nil {
				return fmt.Errorf("invalid paymentaddr: %w", err)
			}
			if len(paymentAddr) != 32 {
				return fmt.Errorf("paymentaddr must be 32 bytes")
			}
		}

		switch {
		case stateless && len(paymentAddr) == 0:
			return fmt.Errorf("--paymentaddr is required for stateless " +
				"cooperative recovery")

		case !stateless && len(paymentAddr) != 0:
			return fmt.Errorf("--paymentaddr is only used for " +
				"stateless recovery")
		}

		cooperativeSweep = &looprpc.CooperativeSweep{
			PaymentAddress: paymentAddr,
		}
	}

	// Call SweepHtlc on loopd trying to sweep the HTLC.
	resp, err := client.SweepHtlc(ctx, &looprpc.SweepHtlcRequest{
		Outpoint:          cmd.String("outpoint"),
		DestAddress:       cmd.String("destaddr"),
		HtlcAddress:       cmd.String("htlcaddr"),
		SatPerVbyte:       uint32(cmd.Uint("feerate")),
		Preimage:          preimage,
		Publish:           cmd.Bool("publish"),
		StatelessRecovery: recovery,
		Cooperative:       cooperativeSweep,
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

// sweepHtlcUint32Flag returns an unsigned integer flag after checking that
// protobuf encoding cannot truncate it.
func sweepHtlcUint32Flag(cmd *cli.Command, name string) (uint32, error) {
	const maxUint32 = 1<<32 - 1

	value := cmd.Uint(name)
	if uint64(value) > maxUint32 {
		return 0, fmt.Errorf("--%s is outside the uint32 range", name)
	}

	return uint32(value), nil
}

// sweepHtlcInt32Flag returns an integer flag after checking that protobuf
// encoding cannot truncate it.
func sweepHtlcInt32Flag(cmd *cli.Command, name string) (int32, error) {
	const (
		minInt32 = -1 << 31
		maxInt32 = 1<<31 - 1
	)

	value := cmd.Int(name)
	if int64(value) < minInt32 || int64(value) > maxInt32 {
		return 0, fmt.Errorf("--%s is outside the int32 range", name)
	}

	return int32(value), nil
}
