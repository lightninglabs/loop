package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/routing/route"
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
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: lastHopFlag.Name,
			Usage: "the pubkey of the last hop to use for the " +
				"quote",
		},
		confTargetFlag,
		verboseFlag,
		privateFlag,
		routeHintsFlag,
	},
	Action: quoteIn,
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

	// Private and routehints are mutually exclusive as setting private
	// means we retrieve our own routehints from the connected node.
	hints, err := validateRouteHints(ctx)
	if err != nil {
		return err
	}

	quoteReq := &looprpc.QuoteRequest{
		Amt:              int64(amt),
		ConfTarget:       int32(ctx.Uint64("conf_target")),
		LoopInRouteHints: hints,
		Private:          ctx.Bool(privateFlag.Name),
	}

	if ctx.IsSet(lastHopFlag.Name) {
		lastHopVertex, err := route.NewVertexFromStr(
			ctx.String(lastHopFlag.Name),
		)
		if err != nil {
			return err
		}

		quoteReq.LoopInLastHop = lastHopVertex[:]
	}

	ctxb := context.Background()
	quoteResp, err := client.GetLoopInQuote(ctxb, quoteReq)
	if err != nil {
		return err
	}

	// For loop in, the fee estimation is handed to lnd which tries to
	// construct a real transaction to sample realistic fees to pay to the
	// HTLC. If the wallet doesn't have enough funds to create this TX, we
	// don't want to fail the quote. But the user should still be informed
	// why the fee shows as -1.
	if quoteResp.HtlcPublishFeeSat == int64(loop.MinerFeeEstimationFailed) {
		_, _ = fmt.Fprintf(os.Stderr, "Warning: Miner fee estimation "+
			"not possible, lnd has insufficient funds to "+
			"create a sample transaction for selected "+
			"amount.\n")
	}

	printQuoteInResp(quoteReq, quoteResp, ctx.Bool("verbose"))
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
		verboseFlag,
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

	printQuoteOutResp(quoteReq, quoteResp, ctx.Bool("verbose"))
	return nil
}

func printQuoteInResp(req *looprpc.QuoteRequest,
	resp *looprpc.InQuoteResponse, verbose bool) {

	totalFee := resp.HtlcPublishFeeSat + resp.SwapFeeSat

	fmt.Printf(satAmtFmt, "Send on-chain:", req.Amt)
	fmt.Printf(satAmtFmt, "Receive off-chain:", req.Amt-totalFee)

	switch {
	case req.ExternalHtlc && !verbose:
		// If it's external then we don't know the miner fee hence the
		// total cost.
		fmt.Printf(satAmtFmt, "Loop service fee:", resp.SwapFeeSat)

	case req.ExternalHtlc && verbose:
		fmt.Printf(satAmtFmt, "Loop service fee:", resp.SwapFeeSat)
		fmt.Println()
		fmt.Printf(blkFmt, "CLTV expiry delta:", resp.CltvDelta)

	case verbose:
		fmt.Println()
		fmt.Printf(
			satAmtFmt, "Estimated on-chain fee:",
			resp.HtlcPublishFeeSat,
		)
		fmt.Printf(satAmtFmt, "Loop service fee:", resp.SwapFeeSat)
		fmt.Printf(satAmtFmt, "Estimated total fee:", totalFee)
		fmt.Println()
		fmt.Printf(blkFmt, "Conf target:", resp.ConfTarget)
		fmt.Printf(blkFmt, "CLTV expiry delta:", resp.CltvDelta)
	default:
		fmt.Printf(satAmtFmt, "Estimated total fee:", totalFee)
	}
}

func printQuoteOutResp(req *looprpc.QuoteRequest,
	resp *looprpc.OutQuoteResponse, verbose bool) {

	totalFee := resp.HtlcSweepFeeSat + resp.SwapFeeSat

	fmt.Printf(satAmtFmt, "Send off-chain:", req.Amt)
	fmt.Printf(satAmtFmt, "Receive on-chain:", req.Amt-totalFee)

	if !verbose {
		fmt.Printf(satAmtFmt, "Estimated total fee:", totalFee)
		return
	}

	fmt.Println()
	fmt.Printf(satAmtFmt, "Estimated on-chain fee:", resp.HtlcSweepFeeSat)
	fmt.Printf(satAmtFmt, "Loop service fee:", resp.SwapFeeSat)
	fmt.Printf(satAmtFmt, "Estimated total fee:", totalFee)
	fmt.Println()
	fmt.Printf(satAmtFmt, "No show penalty (prepay):", resp.PrepayAmtSat)
	fmt.Printf(blkFmt, "Conf target:", resp.ConfTarget)
	fmt.Printf(blkFmt, "CLTV expiry delta:", resp.CltvDelta)
	fmt.Printf("%-38s %s\n",
		"Publication deadline:",
		time.Unix(int64(req.SwapPublicationDeadline), 0),
	)
}
