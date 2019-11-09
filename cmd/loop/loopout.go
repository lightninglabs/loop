package main

import (
	"context"
	"fmt"

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
		cli.Uint64Flag{
			Name: "max_swap_routing_fee",
			Usage: "max off-chain swap routing fee",
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

	sweepConfTarget := int32(ctx.Uint64("conf_target"))
	quoteReq := &looprpc.QuoteRequest{
		Amt:        int64(amt),
		ConfTarget: sweepConfTarget,
	}
	quote, err := client.LoopOutQuote(context.Background(), quoteReq)
	if err != nil {
		return err
	}

	limits := getLimits(amt, quote)

	err = displayLimits(swap.TypeOut, amt, limits, false)
	if err != nil {
		return err
	}

	var unchargeChannel uint64
	if ctx.IsSet("channel") {
		unchargeChannel = ctx.Uint64("channel")
	}

	// var maxSwapRoutingFee uint64
	// if ctx.IsSet("max_swap_routing_fee") {
	// 	maxSwapRoutingFee = ctx.Uint64("max_swap_routing_fee")
	// }

	resp, err := client.LoopOut(context.Background(), &looprpc.LoopOutRequest{
		Amt:                 int64(amt),
		Dest:                destAddr,
		MaxMinerFee:         int64(limits.maxMinerFee),
		MaxPrepayAmt:        int64(*limits.maxPrepayAmt),
		MaxSwapFee:          int64(limits.maxSwapFee),
		MaxPrepayRoutingFee: int64(*limits.maxPrepayRoutingFee),
		MaxSwapRoutingFee:   int64(*limits.maxSwapRoutingFee),
		LoopOutChannel:      unchargeChannel,
		SweepConfTarget:     sweepConfTarget,
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
