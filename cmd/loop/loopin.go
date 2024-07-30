package main

import (
	"context"
	"fmt"

	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
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
		Usage: "the target number of blocks the on-chain htlc " +
			"broadcast by the swap client should confirm within",
	}

	labelFlag = cli.StringFlag{
		Name: "label",
		Usage: fmt.Sprintf("an optional label for this swap,"+
			"limited to %v characters. The label may not start "+
			"with our reserved prefix: %v.",
			labels.MaxLength, labels.Reserved),
	}
	routeHintsFlag = cli.StringSliceFlag{
		Name: "route_hints",
		Usage: "route hints that can each be individually used " +
			"to assist in reaching the invoice's destination",
	}
	privateFlag = cli.BoolFlag{
		Name: "private",
		Usage: "generates and passes routehints. Should be used if " +
			"the connected node is only reachable via private " +
			"channels",
	}

	forceFlag = cli.BoolFlag{
		Name: "force, f",
		Usage: "Assumes yes during confirmation. Using this option " +
			"will result in an immediate swap",
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
				Name: "amt",
				Usage: "the amount in satoshis to loop in. " +
					"To check for the minimum and " +
					"maximum amounts to loop " +
					"in please consult \"loop terms\"",
			},
			cli.BoolFlag{
				Name:  "external",
				Usage: "expect htlc to be published externally",
			},
			confTargetFlag,
			lastHopFlag,
			labelFlag,
			forceFlag,
			verboseFlag,
			routeHintsFlag,
			privateFlag,
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

	// Validate our label early so that we can fail before getting a quote.
	label := ctx.String(labelFlag.Name)
	if err := labels.Validate(label); err != nil {
		return err
	}

	var lastHop []byte
	if ctx.IsSet(lastHopFlag.Name) {
		lastHopVertex, err := route.NewVertexFromStr(
			ctx.String(lastHopFlag.Name),
		)
		if err != nil {
			return err
		}

		lastHop = lastHopVertex[:]
	}

	// Private and routehints are mutually exclusive as setting private
	// means we retrieve our own routehints from the connected node.
	hints, err := validateRouteHints(ctx)
	if err != nil {
		return err
	}

	quoteReq := &looprpc.QuoteRequest{
		Amt:              int64(amt),
		ConfTarget:       htlcConfTarget,
		ExternalHtlc:     external,
		LoopInLastHop:    lastHop,
		LoopInRouteHints: hints,
		Private:          ctx.Bool(privateFlag.Name),
	}

	quote, err := client.GetLoopInQuote(context.Background(), quoteReq)
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

	// Skip showing details if configured
	if !(ctx.Bool("force") || ctx.Bool("f")) {
		err = displayInDetails(quoteReq, quote, ctx.Bool("verbose"))
		if err != nil {
			return err
		}
	}

	req := &looprpc.LoopInRequest{
		Amt:            int64(amt),
		MaxMinerFee:    int64(limits.maxMinerFee),
		MaxSwapFee:     int64(limits.maxSwapFee),
		ExternalHtlc:   external,
		HtlcConfTarget: htlcConfTarget,
		Label:          label,
		Initiator:      defaultInitiator,
		LastHop:        lastHop,
		RouteHints:     hints,
		Private:        ctx.Bool(privateFlag.Name),
	}

	resp, err := client.LoopIn(context.Background(), req)
	if err != nil {
		return err
	}

	fmt.Printf("Swap initiated\n")
	fmt.Printf("ID:           %v\n", resp.Id)
	if resp.HtlcAddressP2Tr != "" {
		fmt.Printf("HTLC address (P2TR): %v\n", resp.HtlcAddressP2Tr)
	} else {
		fmt.Printf("HTLC address (P2WSH): %v\n", resp.HtlcAddressP2Wsh)
	}

	if resp.ServerMessage != "" {
		fmt.Printf("Server message: %v\n", resp.ServerMessage)
	}

	fmt.Println()
	fmt.Printf("Run `loop monitor` to monitor progress.\n")

	return nil
}
