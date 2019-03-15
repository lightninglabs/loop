package main

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var quoteCommand = cli.Command{
	Name:        "quote",
	Usage:       "get a quote for the cost of a swap",
	ArgsUsage:   "amt",
	Description: "Allows to determine the cost of a swap up front",
	Action:      quote,
}

func quote(ctx *cli.Context) error {
	// Show command help if no arguments and flags were provided.
	if ctx.NArg() < 1 {
		cli.ShowCommandHelp(ctx, "quote")
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

	ctxb := context.Background()
	resp, err := client.LoopOutQuote(ctxb, &looprpc.QuoteRequest{
		Amt: int64(amt),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
