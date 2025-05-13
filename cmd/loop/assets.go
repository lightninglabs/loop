package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var assetsCommands = cli.Command{

	Name:      "assets",
	ShortName: "a",
	Usage:     "manage asset swaps",
	Description: `
	`,
	Subcommands: []cli.Command{
		assetsOutCommand,
		listOutCommand,
		listAvailableAssetsComand,
	},
}
var (
	assetsOutCommand = cli.Command{
		Name:      "out",
		ShortName: "o",
		Usage:     "swap asset out",
		ArgsUsage: "",
		Description: `
		List all reservations.
	`,
		Flags: []cli.Flag{
			cli.Uint64Flag{
				Name:  "amt",
				Usage: "the amount in satoshis to loop out.",
			},
			cli.StringFlag{
				Name:  "asset_id",
				Usage: "asset_id",
			},
		},
		Action: assetSwapOut,
	}
	listAvailableAssetsComand = cli.Command{
		Name:      "available",
		ShortName: "a",
		Usage:     "list available assets",
		ArgsUsage: "",
		Description: `
		List available assets from the loop server
	`,

		Action: listAvailable,
	}
	listOutCommand = cli.Command{
		Name:      "list",
		ShortName: "l",
		Usage:     "list asset swaps",
		ArgsUsage: "",
		Description: `
		List all reservations.
	`,
		Action: listOut,
	}
)

func assetSwapOut(ctx *cli.Context) error {
	// First set up the swap client itself.
	client, cleanup, err := getAssetsClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	args := ctx.Args()

	var amtStr string
	switch {
	case ctx.IsSet("amt"):
		amtStr = ctx.String("amt")
	case ctx.NArg() > 0:
		amtStr = args[0]
		args = args.Tail() //nolint: wastedassign
	default:
		// Show command help if no arguments and flags were provided.
		return cli.ShowCommandHelp(ctx, "out")
	}

	amt, err := parseAmt(amtStr)
	if err != nil {
		return err
	}
	if amt <= 0 {
		return fmt.Errorf("amount must be greater than zero")
	}

	assetId, err := hex.DecodeString(ctx.String("asset_id"))
	if err != nil {
		return err
	}

	if len(assetId) != 32 {
		return fmt.Errorf("invalid asset id")
	}

	// First we'll list the available assets.
	assets, err := client.ClientListAvailableAssets(
		context.Background(),
		&looprpc.ClientListAvailableAssetsRequest{},
	)
	if err != nil {
		return err
	}

	// We now extract the asset name from the list of available assets.
	var assetName string
	for _, asset := range assets.AvailableAssets {
		if bytes.Equal(asset.AssetId, assetId) {
			assetName = asset.Name
			break
		}
	}
	if assetName == "" {
		return fmt.Errorf("asset not found")
	}

	// First we'll quote the swap out to get the current fee and rate.
	quote, err := client.ClientGetAssetSwapOutQuote(
		context.Background(),
		&looprpc.ClientGetAssetSwapOutQuoteRequest{
			Amt:   uint64(amt),
			Asset: assetId,
		},
	)
	if err != nil {
		return err
	}

	totalSats := (amt * btcutil.Amount(quote.SatsPerUnit)).MulF64(float64(1) + quote.SwapFee)

	fmt.Printf(satAmtFmt, "Fixed prepay cost:", quote.PrepayAmt)
	fmt.Printf(bpsFmt, "Swap fee:", int64(quote.SwapFee*10000))
	fmt.Printf(satAmtFmt, "Sats per unit:", quote.SatsPerUnit)
	fmt.Printf(satAmtFmt, "Swap Offchain payment:", totalSats)
	fmt.Printf(satAmtFmt, "Total Send off-chain:", totalSats+btcutil.Amount(quote.PrepayAmt))
	fmt.Printf(assetFmt, "Receive assets on-chain:", int64(amt), assetName)

	fmt.Println("CONTINUE SWAP? (y/n): ")

	var answer string
	fmt.Scanln(&answer)
	if answer != "y" {
		return errors.New("swap canceled")
	}

	res, err := client.SwapOut(
		context.Background(),
		&looprpc.SwapOutRequest{
			Amt:   uint64(amt),
			Asset: assetId,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(res)
	return nil
}

func listAvailable(ctx *cli.Context) error {
	// First set up the swap client itself.
	client, cleanup, err := getAssetsClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := client.ClientListAvailableAssets(
		context.Background(),
		&looprpc.ClientListAvailableAssetsRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(res)
	return nil
}
func listOut(ctx *cli.Context) error {
	// First set up the swap client itself.
	client, cleanup, err := getAssetsClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := client.ListAssetSwaps(
		context.Background(),
		&looprpc.ListAssetSwapsRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(res)
	return nil
}

func getAssetsClient(ctx *cli.Context) (looprpc.AssetsClientClient, func(), error) {
	rpcServer := ctx.GlobalString("rpcserver")
	tlsCertPath, macaroonPath, err := extractPathArgs(ctx)
	if err != nil {
		return nil, nil, err
	}
	conn, err := getClientConn(rpcServer, tlsCertPath, macaroonPath)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { conn.Close() }

	loopClient := looprpc.NewAssetsClientClient(conn)
	return loopClient, cleanup, nil
}
