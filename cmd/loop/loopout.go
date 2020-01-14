package main

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop"
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
		cli.StringFlag{
			Name: "addr",
			Usage: "the optional address that the looped out funds " +
				"should be sent to, if let blank the funds " +
				"will go to lnd's wallet",
		},
		cli.Uint64Flag{
			Name:  "amt",
			Usage: "the amount in satoshis to loop out",
		},
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the number of blocks from the swap " +
				"initiation height that the on-chain HTLC " +
				"should be swept within",
			Value: uint64(loop.DefaultSweepConfTarget),
		},
		cli.Int64Flag{
			Name: "max_swap_routing_fee",
			Usage: "the max off-chain swap routing fee in satoshis, " +
				"if let blank a default max fee will be used",
		},
		cli.BoolFlag{
			Name: "fast",
			Usage: "Indicate you want to swap immediately, " +
				"paying potentially a higher fee. If not " +
				"set the swap server might choose to wait up " +
				"to 30 minutes before publishing the swap " +
				"HTLC on-chain, to save on chain fees. Not " +
				"setting this flag might result in a lower " +
				"swap fee.",
		},
	},
	Action: loopOut,
}

func loopOut(ctx *cli.Context) error {
	args := ctx.Args()

	var amtStr string
	switch {
	case ctx.IsSet("amt"):
		amtStr = ctx.String("amt")
	case ctx.NArg() > 0:
		amtStr = args[0]
		args = args.Tail()
	default:
		// Show command help if no arguments and flags were provided.
		return cli.ShowCommandHelp(ctx, "out")
	}

	amt, err := parseAmt(amtStr)
	if err != nil {
		return err
	}

	var destAddr string
	switch {
	case ctx.IsSet("addr"):
		destAddr = ctx.String("addr")
	case args.Present():
		destAddr = args.First()
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// Set our maximum swap wait time. If a fast swap is requested we set
	// it to now, otherwise to 30 minutes in the future.
	fast := ctx.Bool("fast")
	swapDeadline := time.Now()
	if !fast {
		swapDeadline = time.Now().Add(defaultSwapWaitTime)
	}

	sweepConfTarget := int32(ctx.Uint64("conf_target"))
	quoteReq := &looprpc.QuoteRequest{
		Amt:                     int64(amt),
		ConfTarget:              sweepConfTarget,
		SwapPublicationDeadline: uint64(swapDeadline.Unix()),
	}
	quote, err := client.LoopOutQuote(context.Background(), quoteReq)
	if err != nil {
		return err
	}

	// Show a warning if a slow swap was requested.
	warning := ""
	if fast {
		warning = "Fast swap requested."
	} else {
		warning = fmt.Sprintf("Regular swap speed requested, it "+
			"might take up to %v for the swap to be executed.",
			defaultSwapWaitTime)
	}

	limits := getLimits(amt, quote)
	// If configured, use the specified maximum swap routing fee.
	if ctx.IsSet("max_swap_routing_fee") {
		*limits.maxSwapRoutingFee = btcutil.Amount(
			ctx.Int64("max_swap_routing_fee"),
		)
	}
	err = displayLimits(swap.TypeOut, amt, limits, false, warning)
	if err != nil {
		return err
	}

	var unchargeChannel uint64
	if ctx.IsSet("channel") {
		unchargeChannel = ctx.Uint64("channel")
	}

	resp, err := client.LoopOut(context.Background(), &looprpc.LoopOutRequest{
		Amt:                     int64(amt),
		Dest:                    destAddr,
		MaxMinerFee:             int64(limits.maxMinerFee),
		MaxPrepayAmt:            int64(*limits.maxPrepayAmt),
		MaxSwapFee:              int64(limits.maxSwapFee),
		MaxPrepayRoutingFee:     int64(*limits.maxPrepayRoutingFee),
		MaxSwapRoutingFee:       int64(*limits.maxSwapRoutingFee),
		LoopOutChannel:          unchargeChannel,
		SweepConfTarget:         sweepConfTarget,
		SwapPublicationDeadline: uint64(swapDeadline.Unix()),
	})
	if err != nil {
		return err
	}

	fmt.Printf("Swap initiated\n")
	fmt.Printf("ID:           %v\n", resp.Id)
	fmt.Printf("HTLC address: %v\n", resp.HtlcAddress)
	fmt.Println()
	fmt.Printf("Run `loop monitor` to monitor progress.\n")

	return nil
}
