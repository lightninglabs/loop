package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var loopOutCommand = cli.Command{
	Name:      "out",
	Usage:     "perform an off-chain to on-chain swap (looping out)",
	ArgsUsage: "amt [addr]",
	Description: `
	Attempts to loop out the target amount into either the backing lnd's
	wallet, or a targeted address.

	The amount is to be specified in satoshis.

	Optionally a BASE58/bech32 encoded bitcoin destination address may be
	specified. If not specified, a new wallet address will be generated.`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "channel",
			Usage: "the comma-separated list of short " +
				"channel IDs of the channels to loop out",
		},
		cli.StringFlag{
			Name: "addr",
			Usage: "the optional address that the looped out funds " +
				"should be sent to, if let blank the funds " +
				"will go to lnd's wallet",
		},
		cli.StringFlag{
			Name: "account",
			Usage: "the name of the account to generate a new " +
				"address from. You can list the names of " +
				"valid accounts in your backing lnd " +
				"instance with \"lncli wallet accounts list\"",
			Value: "",
		},
		cli.StringFlag{
			Name: "account_addr_type",
			Usage: "the address type of the extended public key " +
				"specified in account. Currently only " +
				"pay-to-taproot-pubkey(p2tr) is supported",
			Value: "p2tr",
		},
		cli.Uint64Flag{
			Name: "amt",
			Usage: "the amount in satoshis to loop out. To check " +
				"for the minimum and maximum amounts to loop " +
				"out please consult \"loop terms\"",
		},
		cli.Uint64Flag{
			Name: "htlc_confs",
			Usage: "the number of confirmations (in blocks) " +
				"that we require for the htlc extended by " +
				"the server before we reveal the preimage",
			Value: uint64(loopdb.DefaultLoopOutHtlcConfirmations),
		},
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the number of blocks from the swap " +
				"initiation height that the on-chain HTLC " +
				"should be swept within",
			Value: uint64(loop.DefaultSweepConfTarget),
		},
		cli.Int64Flag{
			Name: "max_swap_routing_fee",
			Usage: "the max off-chain swap routing fee in " +
				"satoshis, if not specified, a default max " +
				"fee will be used",
		},
		cli.BoolFlag{
			Name: "fast",
			Usage: "indicate you want to swap immediately, " +
				"paying potentially a higher fee. If not " +
				"set the swap server might choose to wait up " +
				"to 30 minutes before publishing the swap " +
				"HTLC on-chain, to save on its chain fees. " +
				"Not setting this flag therefore might " +
				"result in a lower swap fee",
		},
		forceFlag,
		labelFlag,
		verboseFlag,
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

	// Parse outgoing channel set. Don't string split if the flag is empty.
	// Otherwise, strings.Split returns a slice of length one with an empty
	// element.
	var outgoingChanSet []uint64
	if ctx.IsSet("channel") {
		chanStrings := strings.Split(ctx.String("channel"), ",")
		for _, chanString := range chanStrings {
			chanID, err := strconv.ParseUint(chanString, 10, 64)
			if err != nil {
				return fmt.Errorf("error parsing channel id "+
					"\"%v\"", chanString)
			}
			outgoingChanSet = append(outgoingChanSet, chanID)
		}
	}

	// Validate our label early so that we can fail before getting a quote.
	label := ctx.String(labelFlag.Name)
	if err := labels.Validate(label); err != nil {
		return err
	}

	if ctx.IsSet("addr") && ctx.IsSet("account") {
		return fmt.Errorf("cannot set --addr and --account at the " +
			"same time. Please specify only one source for a new " +
			"address to sweep the loop amount to")
	}

	var destAddr string
	var account string
	switch {
	case ctx.IsSet("addr"):
		destAddr = ctx.String("addr")

	case ctx.IsSet("account"):
		account = ctx.String("account")

	case args.Present():
		destAddr = args.First()
	}

	if ctx.IsSet("account") != ctx.IsSet("account_addr_type") {
		return fmt.Errorf("cannot set account without specifying " +
			"account address type and vice versa")
	}

	var accountAddrType looprpc.AddressType
	if ctx.IsSet("account_addr_type") {
		switch ctx.String("account_addr_type") {
		case "p2tr":
			accountAddrType = looprpc.AddressType_TAPROOT_PUBKEY

		default:
			return fmt.Errorf("unknown account address type")
		}
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// Set our maximum swap wait time. If a fast swap is requested we set
	// it to now, otherwise to 30 minutes in the future.
	fast := ctx.Bool("fast")
	swapDeadline := time.Now()
	if !fast {
		swapDeadline = time.Now().Add(defaultSwapWaitTime)
	}

	sweepConfTarget := int32(ctx.Uint64("conf_target"))
	htlcConfs := int32(ctx.Uint64("htlc_confs"))
	if htlcConfs == 0 {
		return fmt.Errorf("at least 1 confirmation required for htlcs")
	}

	quoteReq := &looprpc.QuoteRequest{
		Amt:                     int64(amt),
		ConfTarget:              sweepConfTarget,
		SwapPublicationDeadline: uint64(swapDeadline.Unix()),
	}
	quote, err := client.LoopOutQuote(context.Background(), quoteReq)
	if err != nil {
		return err
	}

	// Show a warning if a slow swap was requested.
	var warning string
	if fast {
		warning = "Fast swap requested."
	} else {
		warning = fmt.Sprintf("Regular swap speed requested, it "+
			"might take up to %v for the swap to be executed.",
			defaultSwapWaitTime)
	}

	limits := getOutLimits(amt, quote)
	// If configured, use the specified maximum swap routing fee.
	if ctx.IsSet("max_swap_routing_fee") {
		limits.maxSwapRoutingFee = btcutil.Amount(
			ctx.Int64("max_swap_routing_fee"),
		)
	}

	// Skip showing details if configured
	if !(ctx.Bool("force") || ctx.Bool("f")) {
		err = displayOutDetails(
			limits, warning, quoteReq, quote, ctx.Bool("verbose"),
		)
		if err != nil {
			return err
		}
	}

	resp, err := client.LoopOut(context.Background(), &looprpc.LoopOutRequest{
		Amt:                     int64(amt),
		Dest:                    destAddr,
		Account:                 account,
		AccountAddrType:         accountAddrType,
		MaxMinerFee:             int64(limits.maxMinerFee),
		MaxPrepayAmt:            int64(limits.maxPrepayAmt),
		MaxSwapFee:              int64(limits.maxSwapFee),
		MaxPrepayRoutingFee:     int64(limits.maxPrepayRoutingFee),
		MaxSwapRoutingFee:       int64(limits.maxSwapRoutingFee),
		OutgoingChanSet:         outgoingChanSet,
		SweepConfTarget:         sweepConfTarget,
		HtlcConfirmations:       htlcConfs,
		SwapPublicationDeadline: uint64(swapDeadline.Unix()),
		Label:                   label,
		Initiator:               defaultInitiator,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Swap initiated\n")
	fmt.Printf("ID:             %x\n", resp.IdBytes)
	fmt.Printf("HTLC address:   %v\n", resp.HtlcAddress) // nolint:staticcheck
	if resp.ServerMessage != "" {
		fmt.Printf("Server message: %v\n", resp.ServerMessage)
	}
	fmt.Println()
	fmt.Printf("Run `loop monitor` to monitor progress.\n")

	return nil
}
