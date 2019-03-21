package main

import (
	"context"
	"fmt"

	"github.com/lightninglabs/loop/looprpc"
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
			Name:   "amt",
			Usage:  "the amount in satoshis to loop out",
		},
	},
	Action: loopOut,
}

func loopOut(ctx *cli.Context) error {
	args := ctx.Args()

	var amtStr string
	if ctx.IsSet("amt") {
	   amtStr = ctx.String("amt")
	} else if ctx.NArg() > 0 {
	   amtStr = args[0]
	   args = args.Tail()
	} else {
	   // Show command help if no arguments and flags were provided.
	   cli.ShowCommandHelp(ctx, "out")
	   return nil
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

	quoteReq := &looprpc.QuoteRequest{
		Amt: int64(amt),
	}
	quote, err := client.LoopOutQuote(context.Background(), quoteReq)
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
