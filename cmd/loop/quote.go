package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/taproot-assets/rfqmath"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/urfave/cli/v3"
)

var quoteCommand = &cli.Command{
	Name:     "quote",
	Usage:    "get a quote for the cost of a swap",
	Commands: []*cli.Command{quoteInCommand, quoteOutCommand},
}

var quoteInCommand = &cli.Command{
	Name:      "in",
	Usage:     "get a quote for the cost of a loop in swap",
	ArgsUsage: "amt",
	Description: "Allows to determine the cost of a swap up front." +
		"Either specify an amount or deposit outpoints.",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: lastHopFlag.Name,
			Usage: "the pubkey of the last hop to use for the " +
				"quote",
		},
		confTargetFlag,
		verboseFlag,
		privateFlag,
		routeHintsFlag,
		&cli.StringSliceFlag{
			Name: "deposit_outpoint",
			Usage: "one or more static address deposit outpoints " +
				"to quote for. Deposit outpoints are not to " +
				"be used in combination with an amount. Each" +
				"additional outpoint can be added by " +
				"specifying --deposit_outpoint tx_id:idx",
		},
	},
	Action: quoteIn,
}

func quoteIn(ctx context.Context, cmd *cli.Command) error {
	// Show command help if the incorrect number arguments was provided.
	if cmd.NArg() != 1 && !cmd.IsSet("deposit_outpoint") {
		return showCommandHelp(ctx, cmd)
	}

	var (
		manualAmt        btcutil.Amount
		depositAmt       btcutil.Amount
		depositOutpoints []string
		err              error
	)

	if cmd.NArg() == 1 {
		args := cmd.Args()
		manualAmt, err = parseAmt(args.First())
		if err != nil {
			return err
		}
	}

	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	// Private and routehints are mutually exclusive as setting private
	// means we retrieve our own routehints from the connected node.
	hints, err := validateRouteHints(cmd)
	if err != nil {
		return err
	}

	if cmd.IsSet("deposit_outpoint") {
		depositOutpoints = cmd.StringSlice("deposit_outpoint")
		depositAmt, err = depositAmount(ctx, client, depositOutpoints)
		if err != nil {
			return err
		}
	}

	quoteReq := &looprpc.QuoteRequest{
		Amt:              int64(manualAmt),
		ConfTarget:       int32(cmd.Uint64("conf_target")),
		LoopInRouteHints: hints,
		Private:          cmd.Bool(privateFlag.Name),
		DepositOutpoints: depositOutpoints,
	}

	if cmd.IsSet(lastHopFlag.Name) {
		lastHopVertex, err := route.NewVertexFromStr(
			cmd.String(lastHopFlag.Name),
		)
		if err != nil {
			return err
		}

		quoteReq.LoopInLastHop = lastHopVertex[:]
	}

	quoteResp, err := client.GetLoopInQuote(ctx, quoteReq)
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

	// If the user specified static address deposits, we quoted for their
	// total value and need to display that value instead of the manually
	// selected one.
	if manualAmt == 0 {
		quoteReq.Amt = int64(depositAmt)
	}
	printQuoteInResp(quoteReq, quoteResp, cmd.Bool("verbose"))
	return nil
}

func depositAmount(ctx context.Context, client looprpc.SwapClientClient,
	depositOutpoints []string) (btcutil.Amount, error) {

	addressSummary, err := client.ListStaticAddressDeposits(
		ctx, &looprpc.ListStaticAddressDepositsRequest{
			Outpoints: depositOutpoints,
		},
	)
	if err != nil {
		return 0, err
	}

	var depositAmt btcutil.Amount
	for _, deposit := range addressSummary.FilteredDeposits {
		depositAmt += btcutil.Amount(deposit.Value)
	}

	return depositAmt, nil
}

var quoteOutCommand = &cli.Command{
	Name:        "out",
	Usage:       "get a quote for the cost of a loop out swap",
	ArgsUsage:   "amt",
	Description: "Allows to determine the cost of a swap up front",
	Flags: []cli.Flag{
		&cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the number of blocks from the swap " +
				"initiation height that the on-chain HTLC " +
				"should be swept within in a Loop Out",
			Value: uint64(loop.DefaultSweepConfTarget),
		},
		&cli.BoolFlag{
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

func quoteOut(ctx context.Context, cmd *cli.Command) error {
	// Show command help if the incorrect number arguments was provided.
	if cmd.NArg() != 1 {
		return showCommandHelp(ctx, cmd)
	}

	args := cmd.Args()
	amt, err := parseAmt(args.First())
	if err != nil {
		return err
	}

	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	fast := cmd.Bool("fast")
	swapDeadline := cliClock.Now()
	if !fast {
		swapDeadline = cliClock.Now().Add(defaultSwapWaitTime)
	}

	quoteReq := &looprpc.QuoteRequest{
		Amt:                     int64(amt),
		ConfTarget:              int32(cmd.Uint64("conf_target")),
		SwapPublicationDeadline: uint64(swapDeadline.Unix()),
	}
	quoteResp, err := client.LoopOutQuote(ctx, quoteReq)
	if err != nil {
		return err
	}

	printQuoteOutResp(quoteReq, quoteResp, cmd.Bool("verbose"))
	return nil
}

func printQuoteInResp(req *looprpc.QuoteRequest,
	resp *looprpc.InQuoteResponse, verbose bool) {

	totalFee := resp.HtlcPublishFeeSat + resp.SwapFeeSat
	amt := req.Amt
	if amt == 0 {
		amt = resp.QuotedAmt
	}

	if req.DepositOutpoints != nil {
		fmt.Printf(satAmtFmt, "Previously deposited on-chain:",
			amt)
	} else {
		fmt.Printf(satAmtFmt, "Send on-chain:", amt)
	}
	fmt.Printf(satAmtFmt, "Receive off-chain:", amt-totalFee)

	switch {
	case req.ExternalHtlc && !verbose:
		// If it's external, then we don't know the miner fee hence the
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

	if resp.AssetRfqInfo != nil {
		assetAmtSwap, err := getAssetAmt(
			req.Amt, resp.AssetRfqInfo.SwapAssetRate,
		)
		if err != nil {
			fmt.Printf("Error converting asset amount: %v\n", err)
			return
		}
		exchangeRate := float64(assetAmtSwap) / float64(req.Amt)
		fmt.Printf(assetAmtFmt, "Send off-chain:",
			assetAmtSwap, resp.AssetRfqInfo.AssetName)
		fmt.Printf(rateFmt, "Exchange rate:",
			exchangeRate, resp.AssetRfqInfo.AssetName)
		fmt.Printf(assetAmtFmt, "Limit Send off-chain:",
			resp.AssetRfqInfo.MaxSwapAssetAmt,
			resp.AssetRfqInfo.AssetName)
	} else {
		fmt.Printf(satAmtFmt, "Send off-chain:", req.Amt)
	}

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
	if resp.AssetRfqInfo != nil {
		assetAmtPrepay, err := getAssetAmt(
			resp.PrepayAmtSat, resp.AssetRfqInfo.PrepayAssetRate,
		)
		if err != nil {
			fmt.Printf("Error converting asset amount: %v\n", err)
			return
		}
		fmt.Printf(assetAmtFmt, "No show penalty (prepay):",
			assetAmtPrepay,
			resp.AssetRfqInfo.AssetName)
		fmt.Printf(assetAmtFmt, "Limit no show penalty (prepay):",
			resp.AssetRfqInfo.MaxPrepayAssetAmt,
			resp.AssetRfqInfo.AssetName)
	} else {
		fmt.Printf(satAmtFmt, "No show penalty (prepay):",
			resp.PrepayAmtSat)
	}
	fmt.Printf(blkFmt, "Conf target:", resp.ConfTarget)
	fmt.Printf(blkFmt, "CLTV expiry delta:", resp.CltvDelta)
	fmt.Printf("%-38s %s\n",
		"Publication deadline:",
		time.Unix(int64(req.SwapPublicationDeadline), 0),
	)
}

// getAssetAmt returns the asset amount for the given amount in satoshis and
// the asset rate.
func getAssetAmt(amt int64, assetRate *looprpc.FixedPoint) (
	uint64, error) {

	askAssetRate, err := unmarshalFixedPoint(assetRate)
	if err != nil {
		return 0, err
	}

	msatAmt := lnwire.MilliSatoshi((amt * 1000))

	assetAmt := rfqmath.MilliSatoshiToUnits(msatAmt, *askAssetRate)

	return assetAmt.ToUint64(), nil
}

// unmarshalFixedPoint converts an RPC FixedPoint to a BigIntFixedPoint.
func unmarshalFixedPoint(fp *looprpc.FixedPoint) (*rfqmath.BigIntFixedPoint,
	error) {

	// convert the looprpc.FixedPoint to a rfqrpc.FixedPoint
	rfqrpcFP := &rfqrpc.FixedPoint{
		Coefficient: fp.Coefficient,
		Scale:       fp.Scale,
	}

	return rpcutils.UnmarshalRfqFixedPoint(rfqrpcFP)
}
