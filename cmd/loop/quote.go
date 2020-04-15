package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var quoteCommand = cli.Command{
	Name:        "quote",
	Usage:       "get a quote for the cost of a swap",
	Subcommands: []cli.Command{quoteInCommand, quoteOutCommand},
}

var quoteInCommand = cli.Command{
	Name:        "in",
	Usage:       "get a quote for the cost of a loop in swap",
	ArgsUsage:   "amt",
	Description: "Allows to determine the cost of a swap up front",
	Flags:       []cli.Flag{confTargetFlag},
	Action:      quoteIn,
}

func quoteIn(ctx *cli.Context) error {
	// Show command help if the incorrect number arguments was provided.
	if ctx.NArg() != 1 {
		return cli.ShowCommandHelp(ctx, "in")
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
	quoteReq := &looprpc.QuoteRequest{
		Amt:        int64(amt),
		ConfTarget: int32(ctx.Uint64("conf_target")),
	}
	quoteResp, err := client.GetLoopInQuote(ctxb, quoteReq)
	if err != nil {
		return err
	}

	// For loop in, the fee estimation is handed to lnd which tries to
	// construct a real transaction to sample realistic fees to pay to the
	// HTLC. If the wallet doesn't have enough funds to create this TX, we
	// don't want to fail the quote. But the user should still be informed
	// why the fee shows as -1.
	if quoteResp.MinerFee == int64(loop.MinerFeeEstimationFailed) {
		_, _ = fmt.Fprintf(os.Stderr, "Warning: Miner fee estimation "+
			"not possible, lnd has insufficient funds to "+
			"create a sample transaction for selected "+
			"amount.\n")
	}

	printRespJSON(quoteResp)
	return nil
}

var quoteOutCommand = cli.Command{
	Name:        "out",
	Usage:       "get a quote for the cost of a loop out swap",
	ArgsUsage:   "amt",
	Description: "Allows to determine the cost of a swap up front",
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the number of blocks from the swap " +
				"initiation height that the on-chain HTLC " +
				"should be swept within in a Loop Out",
			Value: uint64(loop.DefaultSweepConfTarget),
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
	Action: quoteOut,
}

func quoteOut(ctx *cli.Context) error {
	// Show command help if the incorrect number arguments was provided.
	if ctx.NArg() != 1 {
		return cli.ShowCommandHelp(ctx, "out")
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
	quoteReq := &looprpc.QuoteRequest{
		Amt:                     int64(amt),
		ConfTarget:              int32(ctx.Uint64("conf_target")),
		SwapPublicationDeadline: uint64(swapDeadline.Unix()),
	}
	quoteResp, err := client.LoopOutQuote(ctxb, quoteReq)
	if err != nil {
		return err
	}

	printRespJSON(quoteResp)
	return nil
}
