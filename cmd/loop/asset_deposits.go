package main

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var (
	assetDepositsCommands = cli.Command{
		Name:      "asset deposits",
		ShortName: "ad",
		Usage:     "TAP asset deposit commands.",
		Subcommands: []cli.Command{
			newAssetDepositCommand,
			listAssetDepositsCommand,
			withdrawAssetDepositCommand,
			testCoSignCommand,
			testKeyRevealCommand,
		},
	}

	newAssetDepositCommand = cli.Command{
		Name:        "new",
		ShortName:   "n",
		Usage:       "Create a new TAP asset deposit.",
		Description: "Create a new TAP asset deposit.",
		Action:      newAssetDeposit,
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "asset_id",
				Usage: "The asset id of the asset to deposit.",
			},
			cli.Uint64Flag{
				Name: "amt",
				Usage: "the amount to deposit (in asset " +
					"units).",
			},
			cli.UintFlag{
				Name:  "expiry",
				Usage: "the deposit expiry in blocks.",
			},
		},
	}

	listAssetDepositsCommand = cli.Command{
		Name:        "list",
		ShortName:   "l",
		Usage:       "List TAP asset deposits.",
		Description: "List TAP asset deposits.",
		Flags: []cli.Flag{
			cli.UintFlag{
				Name: "min_confs",
				Usage: "The minimum amount of confirmations " +
					"an anchor output should have to be " +
					"listed.",
			},
			cli.UintFlag{
				Name: "max_confs",
				Usage: "The maximum number of confirmations " +
					"an anchor output could have to be " +
					"listed.",
			},
		},
		Action: listAssetDeposits,
	}

	withdrawAssetDepositCommand = cli.Command{
		Name:        "withdraw",
		ShortName:   "w",
		Usage:       "Withdraw TAP asset deposits.",
		Description: "Withdraw TAP asset deposits.",
		Action:      withdrawAssetDeposit,
		Flags: []cli.Flag{
			cli.StringSliceFlag{
				Name: "deposit_ids",
				Usage: "The deposit ids of the asset " +
					"deposits to withdraw.",
			},
		},
	}

	testKeyRevealCommand = cli.Command{
		Name:      "testkeyreveal",
		ShortName: "tkr",
		Usage:     "Test revealing the key of a deposit to the server.",
		Action:    testKeyReveal,
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "deposit_id",
				Usage: "The deposit id of the asset deposit.",
			},
		},
	}

	testCoSignCommand = cli.Command{
		Name:      "testcosign",
		ShortName: "tcs",
		Usage:     "Test co-signing a deposit to spend to an HTLC.",
		Action:    testCoSign,
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "deposit_id",
				Usage: "The deposit id of the asset deposit.",
			},
		},
	}
)

func init() {
	commands = append(commands, assetDepositsCommands)
}

func newAssetDeposit(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "newdeposit")
	}

	client, cleanup, err := getAssetDepositsClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	assetID := ctx.String("asset_id")
	amt := ctx.Uint64("amt")
	expiry := int32(ctx.Uint("expiry"))

	resp, err := client.NewAssetDeposit(
		ctxb, &looprpc.NewAssetDepositRequest{
			AssetId:   assetID,
			Amount:    amt,
			CsvExpiry: expiry,
		},
	)
	if err != nil {
		return err
	}

	printJSON(resp)

	return nil
}

func listAssetDeposits(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "list")
	}

	client, cleanup, err := getAssetDepositsClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListAssetDeposits(
		ctxb, &looprpc.ListAssetDepositsRequest{
			MinConfs: uint32(ctx.Int("min_confs")),
			MaxConfs: uint32(ctx.Int("max_confs")),
		})
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

func withdrawAssetDeposit(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "withdraw")
	}

	client, cleanup, err := getAssetDepositsClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	depositIDs := ctx.StringSlice("deposit_ids")

	resp, err := client.WithdrawAssetDeposits(
		ctxb, &looprpc.WithdrawAssetDepositsRequest{
			DepositIds: depositIDs,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

func testKeyReveal(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "testkeyreveal")
	}

	client, cleanup, err := getAssetDepositsClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	depositID := ctx.String("deposit_id")

	resp, err := client.RevealAssetDepositKey(
		ctxb, &looprpc.RevealAssetDepositKeyRequest{
			DepositId: depositID,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

func testCoSign(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "testcosign")
	}

	client, cleanup, err := getAssetDepositsClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	depositID := ctx.String("deposit_id")

	resp, err := client.TestCoSignAssetDepositHTLC(
		ctxb, &looprpc.TestCoSignAssetDepositHTLCRequest{
			DepositId: depositID,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
