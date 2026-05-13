package main

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

var recoverCommand = &cli.Command{
	Name:  "recover",
	Usage: "restore static address and L402 state from a local backup file",
	Description: "Restores the local static-address state and L402 token " +
		"from an encrypted backup file. If --backup_file is omitted, " +
		"loopd selects the latest decryptable active-network backup " +
		"candidate and fully validates it before restoring state.",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "backup_file",
			Usage: "path to an encrypted backup file; if omitted, " +
				"loopd selects and validates the latest active-network " +
				"backup candidate",
		},
	},
	Action: runRecover,
}

var recoverDepositCommand = &cli.Command{
	Name:  "recoverdeposit",
	Usage: "recover one static address deposit from on-chain data",
	Description: "Verifies the provided transaction output on-chain, " +
		"restores the matching static address, stores the deposit, and " +
		"starts normal deposit tracking.",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:     "txid",
			Usage:    "transaction ID containing the deposit output",
			Required: true,
		},
		&cli.UintFlag{
			Name:     "vout",
			Usage:    "deposit output index",
			Required: true,
		},
		&cli.IntFlag{
			Name:     "height_hint",
			Usage:    "block height hint for the deposit transaction",
			Required: true,
		},
		&cli.StringFlag{
			Name:     "pkscript_hex",
			Usage:    "expected static address P2TR pkScript in hex",
			Required: true,
		},
	},
	Action: runRecoverDeposit,
}

func runRecover(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.Recover(
		ctx, &looprpc.RecoverRequest{
			BackupFile: cmd.String("backup_file"),
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

// runRecoverDeposit calls the daemon to manually recover one static-address
// deposit from caller-supplied on-chain coordinates.
func runRecoverDeposit(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.RecoverDeposit(
		ctx, &looprpc.RecoverDepositRequest{
			Txid:        cmd.String("txid"),
			Vout:        uint32(cmd.Uint("vout")),
			HeightHint:  int32(cmd.Int("height_hint")),
			PkscriptHex: cmd.String("pkscript_hex"),
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
