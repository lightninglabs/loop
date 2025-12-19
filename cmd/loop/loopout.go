package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

var (
	channelFlag = &cli.StringFlag{
		Name: "channel",
		Usage: "the comma-separated list of short " +
			"channel IDs of the channels to loop out",
	}
)
var loopOutCommand = &cli.Command{
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
		&cli.StringFlag{
			Name: "addr",
			Usage: "the optional address that the looped out funds " +
				"should be sent to, if let blank the funds " +
				"will go to lnd's wallet",
		},
		&cli.StringFlag{
			Name: "account",
			Usage: "the name of the account to generate a new " +
				"address from. You can list the names of " +
				"valid accounts in your backing lnd " +
				"instance with \"lncli wallet accounts list\"",
			Value: "",
		},
		&cli.StringFlag{
			Name: "account_addr_type",
			Usage: "the address type of the extended public key " +
				"specified in account. Currently only " +
				"pay-to-taproot-pubkey(p2tr) is supported",
			Value: "p2tr",
		},
		&cli.Uint64Flag{
			Name: "amt",
			Usage: "the amount in satoshis to loop out. To check " +
				"for the minimum and maximum amounts to loop " +
				"out please consult \"loop terms\"",
		},
		&cli.Uint64Flag{
			Name: "htlc_confs",
			Usage: "the number of confirmations (in blocks) " +
				"that we require for the htlc extended by " +
				"the server before we reveal the preimage",
			Value: uint64(loopdb.DefaultLoopOutHtlcConfirmations),
		},
		&cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the number of blocks from the swap " +
				"initiation height that the on-chain HTLC " +
				"should be swept within",
			Value: uint64(loop.DefaultSweepConfTarget),
		},
		&cli.Int64Flag{
			Name: "max_swap_routing_fee",
			Usage: "the max off-chain swap routing fee in " +
				"satoshis, if not specified, a default max " +
				"fee will be used",
		},
		&cli.BoolFlag{
			Name: "fast",
			Usage: "indicate you want to swap immediately, " +
				"paying potentially a higher fee. If not " +
				"set the swap server might choose to wait up " +
				"to 30 minutes before publishing the swap " +
				"HTLC on-chain, to save on its chain fees. " +
				"Not setting this flag therefore might " +
				"result in a lower swap fee",
		},
		&cli.DurationFlag{
			Name: "payment_timeout",
			Usage: "the timeout for each individual off-chain " +
				"payment attempt. If not set, the default " +
				"timeout of 1 hour will be used. As the " +
				"payment might be retried, the actual total " +
				"time may be longer",
		},
		&cli.StringFlag{
			Name: "asset_id",
			Usage: "the asset ID of the asset to loop out, " +
				"if this is set, the loop daemon will " +
				"require a connection to a taproot assets " +
				"daemon",
		},
		&cli.StringFlag{
			Name: "asset_edge_node",
			Usage: "the pubkey of the edge node of the asset to " +
				"loop out, this is required if the taproot " +
				"assets daemon has multiple channels of the " +
				"given asset id with different edge nodes",
		},
		forceFlag,
		labelFlag,
		verboseFlag,
		channelFlag,
	},
	Action: loopOut,
}

func loopOut(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args()

	var (
		amtStr    string
		remaining []string
	)
	switch {
	case cmd.IsSet("amt"):
		amtStr = strconv.FormatUint(cmd.Uint64("amt"), 10)
	case cmd.NArg() == 1 || cmd.NArg() == 2:
		amtStr = args.First()
		remaining = args.Tail()
	default:
		// Show command help if no arguments and flags were provided.
		return showCommandHelp(ctx, cmd)
	}

	amt, err := parseAmt(amtStr)
	if err != nil {
		return err
	}

	// Parse outgoing channel set. Don't string split if the flag is empty.
	// Otherwise, strings.Split returns a slice of length one with an empty
	// element.
	var outgoingChanSet []uint64
	if cmd.IsSet("channel") {
		if cmd.IsSet("asset_id") {
			return fmt.Errorf("channel flag is not supported when " +
				"looping out assets")
		}
		chanStrings := strings.Split(cmd.String("channel"), ",")
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
	label := cmd.String(labelFlag.Name)
	if err := labels.Validate(label); err != nil {
		return err
	}

	if cmd.IsSet("addr") && cmd.IsSet("account") {
		return fmt.Errorf("cannot set --addr and --account at the " +
			"same time. Please specify only one source for a new " +
			"address to sweep the loop amount to")
	}

	var (
		destAddr string
		account  string
	)
	switch {
	case cmd.IsSet("addr"):
		destAddr = cmd.String("addr")

	case cmd.IsSet("account"):
		account = cmd.String("account")

	case len(remaining) > 0:
		destAddr = remaining[0]
	}

	if cmd.IsSet("account") != cmd.IsSet("account_addr_type") {
		return fmt.Errorf("cannot set account without specifying " +
			"account address type and vice versa")
	}

	var accountAddrType looprpc.AddressType
	if cmd.IsSet("account_addr_type") {
		switch cmd.String("account_addr_type") {
		case "p2tr":
			accountAddrType = looprpc.AddressType_TAPROOT_PUBKEY

		default:
			return fmt.Errorf("unknown account address type")
		}
	}

	var assetLoopOutInfo *looprpc.AssetLoopOutRequest

	var assetId []byte
	if cmd.IsSet("asset_id") {
		if !cmd.IsSet("asset_edge_node") {
			return fmt.Errorf("asset edge node is required when " +
				"assetid is set")
		}

		assetId, err = hex.DecodeString(cmd.String("asset_id"))
		if err != nil {
			return err
		}

		assetEdgeNode, err := hex.DecodeString(
			cmd.String("asset_edge_node"),
		)
		if err != nil {
			return err
		}

		assetLoopOutInfo = &looprpc.AssetLoopOutRequest{
			AssetId:       assetId,
			AssetEdgeNode: assetEdgeNode,
		}
	}

	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	// Set our maximum swap wait time. If a fast swap is requested we set
	// it to now, otherwise to 30 minutes in the future.
	fast := cmd.Bool("fast")
	swapDeadline := time.Now()
	if !fast {
		swapDeadline = time.Now().Add(defaultSwapWaitTime)
	}

	sweepConfTarget := int32(cmd.Uint64("conf_target"))
	htlcConfs := int32(cmd.Uint64("htlc_confs"))
	if htlcConfs == 0 {
		return fmt.Errorf("at least 1 confirmation required for htlcs")
	}

	quoteReq := &looprpc.QuoteRequest{
		Amt:                     int64(amt),
		ConfTarget:              sweepConfTarget,
		SwapPublicationDeadline: uint64(swapDeadline.Unix()),
		AssetInfo:               assetLoopOutInfo,
	}
	quote, err := client.LoopOutQuote(ctx, quoteReq)
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
	if cmd.IsSet("max_swap_routing_fee") {
		limits.maxSwapRoutingFee = btcutil.Amount(
			cmd.Int64("max_swap_routing_fee"),
		)
	}

	// Skip showing details if configured
	if !(cmd.Bool("force") || cmd.Bool("f")) {
		err = displayOutDetails(
			limits, warning, quoteReq, quote, cmd.Bool("verbose"),
		)
		if err != nil {
			return err
		}
	}

	var paymentTimeout int64
	if cmd.IsSet("payment_timeout") {
		parsedTimeout := cmd.Duration("payment_timeout")
		if parsedTimeout.Truncate(time.Second) != parsedTimeout {
			return fmt.Errorf("payment timeout must be a " +
				"whole number of seconds")
		}

		paymentTimeout = int64(parsedTimeout.Seconds())
		if paymentTimeout <= 0 {
			return fmt.Errorf("payment timeout must be a " +
				"positive value")
		}

		if paymentTimeout > math.MaxUint32 {
			return fmt.Errorf("payment timeout is too large")
		}
	}

	resp, err := client.LoopOut(ctx, &looprpc.LoopOutRequest{
		Amt:                     int64(amt),
		Dest:                    destAddr,
		IsExternalAddr:          destAddr != "",
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
		PaymentTimeout:          uint32(paymentTimeout),
		AssetInfo:               assetLoopOutInfo,
		AssetRfqInfo:            quote.AssetRfqInfo,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Swap initiated\n")
	fmt.Printf("ID:             %x\n", resp.IdBytes)
	if resp.ServerMessage != "" {
		fmt.Printf("Server message: %v\n", resp.ServerMessage)
	}
	fmt.Println()
	fmt.Printf("Run `loop monitor` to monitor progress.\n")

	return nil
}
