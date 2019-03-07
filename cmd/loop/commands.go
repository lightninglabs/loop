package main

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/urfave/cli"
)

var loopOutCommand = cli.Command{
	Name:      "out",
	Usage:     "perform an off-chain to on-chain swap (looping out)",
	ArgsUsage: "amt [addr]",
	Description: `
	Attempts loop out the target amount into either the backing lnd's
	wallet, or a targeted address. 
	
	The amount is to be specified in satoshis. 

	Optionally a BASE58/bech32 encoded bitcoin destination address may be 
	specified. If not specified, a new wallet address will be generated.`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name:  "channel",
			Usage: "the 8-byte compact channel ID of the channel to loop out",
		},
	},
	Action: loopOut,
}

func loopOut(ctx *cli.Context) error {
	// Show command help if no arguments and flags were provided.
	if ctx.NArg() < 1 {
		cli.ShowCommandHelp(ctx, "out")
		return nil
	}

	args := ctx.Args()

	amt, err := parseAmt(args[0])
	if err != nil {
		return err
	}

	var destAddr string
	args = args.Tail()
	if args.Present() {
		destAddr = args.First()
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	quote, err := client.GetLoopOutQuote(
		context.Background(),
		&looprpc.QuoteRequest{
			Amt: int64(amt),
		},
	)
	if err != nil {
		return err
	}

	limits := getLimits(amt, quote)

	if err := displayLimits(amt, limits); err != nil {
		return err
	}

	var unchargeChannel uint64
	if ctx.IsSet("channel") {
		unchargeChannel = ctx.Uint64("channel")
	}

	resp, err := client.LoopOut(context.Background(), &looprpc.LoopOutRequest{
		Amt:                 int64(amt),
		Dest:                destAddr,
		MaxMinerFee:         int64(limits.maxMinerFee),
		MaxPrepayAmt:        int64(limits.maxPrepayAmt),
		MaxSwapFee:          int64(limits.maxSwapFee),
		MaxPrepayRoutingFee: int64(limits.maxPrepayRoutingFee),
		MaxSwapRoutingFee:   int64(limits.maxSwapRoutingFee),
		LoopOutChannel:      unchargeChannel,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Swap initiated with id: %v\n", resp.Id[:8])
	fmt.Printf("Run `loop monitor` to monitor progress.\n")

	return nil
}

var termsCommand = cli.Command{
	Name:   "terms",
	Usage:  "show current server swap terms",
	Action: terms,
}

func terms(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	terms, err := client.GetLoopOutTerms(
		context.Background(), &looprpc.TermsRequest{},
	)
	if err != nil {
		return err
	}

	fmt.Printf("Amount: %d - %d\n",
		btcutil.Amount(terms.MinSwapAmount),
		btcutil.Amount(terms.MaxSwapAmount),
	)
	if err != nil {
		return err
	}

	printTerms := func(terms *looprpc.TermsResponse) {
		fmt.Printf("Amount: %d - %d\n",
			btcutil.Amount(terms.MinSwapAmount),
			btcutil.Amount(terms.MaxSwapAmount),
		)
		fmt.Printf("Fee:    %d + %.4f %% (%d prepaid)\n",
			btcutil.Amount(terms.SwapFeeBase),
			swap.FeeRateAsPercentage(terms.SwapFeeRate),
			btcutil.Amount(terms.PrepayAmt),
		)

		fmt.Printf("Cltv delta:   %v blocks\n", terms.CltvDelta)
	}

	fmt.Println("Loop Out")
	fmt.Println("--------")
	printTerms(terms)

	return nil
}

var monitorCommand = cli.Command{
	Name:        "monitor",
	Usage:       "monitor progress of any active swaps",
	Description: "Allows the user to monitor progress of any active swaps",
	Action:      monitor,
}

func monitor(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	stream, err := client.Monitor(
		context.Background(), &looprpc.MonitorRequest{})
	if err != nil {
		return err
	}

	for {
		swap, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv: %v", err)
		}
		logSwap(swap)
	}
}
