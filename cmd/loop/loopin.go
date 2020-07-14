package main

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/urfave/cli"
)

var (
	lastHopFlag = cli.StringFlag{
		Name:  "last_hop",
		Usage: "the pubkey of the last hop to use for this swap",
	}

	confTargetFlag = cli.Uint64Flag{
		Name: "conf_target",
		Usage: "the target number of blocks the on-chain " +
			"htlc broadcast by the swap client should " +
			"confirm within",
	}

	loopInCommand = cli.Command{
		Name:      "in",
		Usage:     "perform an on-chain to off-chain swap (loop in)",
		ArgsUsage: "amt",
		Description: `
		Send the amount in satoshis specified by the amt argument 
		off-chain.
		
		By default the swap client will create and broadcast the 
		on-chain htlc. The fee priority of this transaction can 
		optionally be set using the conf_target flag. 

		The external flag can be set to publish the on chain htlc 
		independently. Note that this flag cannot be set with the 
		conf_target flag.
		`,
		Flags: []cli.Flag{
			cli.Uint64Flag{
				Name:  "amt",
				Usage: "the amount in satoshis to loop in",
			},
			cli.BoolFlag{
				Name:  "external",
				Usage: "expect htlc to be published externally",
			},
			confTargetFlag,
			lastHopFlag,
		},
		Action: loopIn,
	}
)

func loopIn(ctx *cli.Context) error {
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
		return cli.ShowCommandHelp(ctx, "in")
	}

	amt, err := parseAmt(amtStr)
	if err != nil {
		return err
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	external := ctx.Bool("external")
	htlcConfTarget := int32(ctx.Uint64(confTargetFlag.Name))

	// External and confirmation target are mutually exclusive; either the
	// on chain htlc is being externally broadcast, or we are creating the
	// on chain htlc with a desired confirmation target. Fail if both are
	// set.
	if external && htlcConfTarget != 0 {
		return fmt.Errorf("external and conf_target both set")
	}

	quote, err := client.GetLoopInQuote(
		context.Background(),
		&looprpc.QuoteRequest{
			Amt:          int64(amt),
			ConfTarget:   htlcConfTarget,
			ExternalHtlc: external,
		},
	)
	if err != nil {
		return err
	}

	// For loop in, the fee estimation is handed to lnd which tries to
	// construct a real transaction to sample realistic fees to pay to the
	// HTLC. If the wallet doesn't have enough funds to create this TX, we
	// know it won't have enough to pay the real transaction either. It
	// makes sense to abort the loop in this case.
	if !external && quote.HtlcPublishFeeSat ==
		int64(loop.MinerFeeEstimationFailed) {

		return fmt.Errorf("miner fee estimation not " +
			"possible, lnd has insufficient funds to " +
			"create a sample transaction for selected " +
			"amount")
	}

	limits := getInLimits(quote)
	err = displayInLimits(
		amt, btcutil.Amount(quote.HtlcPublishFeeSat), limits, external,
	)
	if err != nil {
		return err
	}

	req := &looprpc.LoopInRequest{
		Amt:            int64(amt),
		MaxMinerFee:    int64(limits.maxMinerFee),
		MaxSwapFee:     int64(limits.maxSwapFee),
		ExternalHtlc:   external,
		HtlcConfTarget: htlcConfTarget,
	}

	if ctx.IsSet(lastHopFlag.Name) {
		lastHop, err := route.NewVertexFromStr(
			ctx.String(lastHopFlag.Name),
		)
		if err != nil {
			return err
		}

		req.LastHop = lastHop[:]
	}

	resp, err := client.LoopIn(context.Background(), req)
	if err != nil {
		return err
	}

	fmt.Printf("Swap initiated\n")
	fmt.Printf("ID:           %v\n", resp.Id)
	if external {
		fmt.Printf("HTLC address (NP2WSH): %v\n", resp.HtlcAddressNp2Wsh)
	}
	fmt.Printf("HTLC address (P2WSH): %v\n", resp.HtlcAddressP2Wsh)
	if resp.ServerMessage != "" {
		fmt.Printf("Server message: %v\n", resp.ServerMessage)
	}
	fmt.Println()
	fmt.Printf("Run `loop monitor` to monitor progress.\n")

	return nil
}
