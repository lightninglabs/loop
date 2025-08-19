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
