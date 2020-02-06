package main

import (
	"context"
	"time"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var quoteCommand = cli.Command{
	Name:        "quote",
	Usage:       "get a quote for the cost of a swap",
	ArgsUsage:   "amt",
	Description: "Allows to determine the cost of a swap up front",
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the number of blocks from the swap " +
				"initiation height that the on-chain HTLC " +
				"should be swept within in a Loop Out",
			Value: 6,
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
	Action: quote,
}

func quote(ctx *cli.Context) error {
	// Show command help if the incorrect number arguments was provided.
	if ctx.NArg() != 1 {
		return cli.ShowCommandHelp(ctx, "quote")
	}

	args := ctx.Args()
	amt, err := parseAmt(args[0])
	if err != nil {
		return err
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	fast := ctx.Bool("fast")
	swapDeadline := time.Now()
	if !fast {
		swapDeadline = time.Now().Add(defaultSwapWaitTime)
	}

	ctxb := context.Background()
	resp, err := client.LoopOutQuote(ctxb, &looprpc.QuoteRequest{
		Amt:                     int64(amt),
		ConfTarget:              int32(ctx.Uint64("conf_target")),
		SwapPublicationDeadline: uint64(swapDeadline.Unix()),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
