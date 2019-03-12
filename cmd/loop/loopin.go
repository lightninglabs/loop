package main

import (
	"context"
	"fmt"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var loopInCommand = cli.Command{
	Name:      "in",
	Usage:     "perform an on-chain to off-chain swap (loop in)",
	ArgsUsage: "amt",
	Description: `
		Send the amount in satoshis specified by the amt argument off-chain.`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name:  "channel",
			Usage: "the 8-byte compact channel ID of the channel to loop in",
		},
	},
	Action: loopIn,
}

func loopIn(ctx *cli.Context) error {
	// Show command help if no arguments and flags were provided.
	if ctx.NArg() < 1 {
		cli.ShowCommandHelp(ctx, "in")
		return nil
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

	quote, err := client.GetLoopInQuote(
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

	var loopInChannel uint64
	if ctx.IsSet("channel") {
		loopInChannel = ctx.Uint64("channel")
	}

	resp, err := client.LoopIn(context.Background(), &looprpc.LoopInRequest{
		Amt:                 int64(amt),
		MaxMinerFee:         int64(limits.maxMinerFee),
		MaxPrepayAmt:        int64(limits.maxPrepayAmt),
		MaxSwapFee:          int64(limits.maxSwapFee),
		MaxPrepayRoutingFee: int64(limits.maxPrepayRoutingFee),
		LoopInChannel:       loopInChannel,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Swap initiated with id: %v\n", resp.Id[:8])
	fmt.Printf("Run swapcli without a command to monitor progress.\n")

	return nil
}
