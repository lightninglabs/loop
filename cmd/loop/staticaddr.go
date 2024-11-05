package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/urfave/cli"
)

var staticAddressCommands = cli.Command{
	Name:      "static",
	ShortName: "s",
	Usage:     "perform on-chain to off-chain swaps using static addresses.",
	Subcommands: []cli.Command{
		newStaticAddressCommand,
		listUnspentCommand,
		listDepositsCommand,
		listStaticAddressSwapsCommand,
		withdrawalCommand,
		summaryCommand,
	},
	Description: `
	Requests a loop-in swap based on static address deposits. After the
	creation of a static address funds can be send to it. Once the funds are
	confirmed on-chain they can be swapped instantaneously. If deposited
	funds are not needed they can we withdrawn back to the local lnd wallet.
	`,
	Flags: []cli.Flag{
		cli.StringSliceFlag{
			Name: "utxo",
			Usage: "specify the utxos of deposits as " +
				"outpoints(tx:idx) that should be looped in.",
		},
		cli.BoolFlag{
			Name:  "all",
			Usage: "loop in all static address deposits.",
		},
		cli.DurationFlag{
			Name: "payment_timeout",
			Usage: "the maximum time in seconds that the server " +
				"is allowed to take for the swap payment. " +
				"The client can retry the swap with adjusted " +
				"parameters after the payment timed out.",
		},
		lastHopFlag,
		labelFlag,
		routeHintsFlag,
		privateFlag,
		forceFlag,
		verboseFlag,
	},
	Action: staticAddressLoopIn,
}

var newStaticAddressCommand = cli.Command{
	Name:      "new",
	ShortName: "n",
	Usage:     "Create a new static loop in address.",
	Description: `
	Requests a new static loop in address from the server. Funds that are
	sent to this address will be locked by a 2:2 multisig between us and the
	loop server, or a timeout path that we can sweep once it opens up. The 
	funds can either be cooperatively spent with a signature from the server
	or looped in.
	`,
	Action: newStaticAddress,
}

func newStaticAddress(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "new")
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.NewStaticAddress(
		ctxb, &looprpc.NewStaticAddressRequest{},
	)
	if err != nil {
		return err
	}

	fmt.Printf("Received a new static loop-in address from the server: "+
		"%s\n", resp.Address)

	return nil
}

var listUnspentCommand = cli.Command{
	Name:      "listunspent",
	ShortName: "l",
	Usage:     "List unspent static address outputs.",
	Description: `
	List all unspent static address outputs.
	`,
	Flags: []cli.Flag{
		cli.IntFlag{
			Name: "min_confs",
			Usage: "The minimum amount of confirmations an " +
				"output should have to be listed.",
		},
		cli.IntFlag{
			Name: "max_confs",
			Usage: "The maximum number of confirmations an " +
				"output could have to be listed.",
		},
	},
	Action: listUnspent,
}

func listUnspent(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "listunspent")
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListUnspentDeposits(
		ctxb, &looprpc.ListUnspentDepositsRequest{
			MinConfs: int32(ctx.Int("min_confs")),
			MaxConfs: int32(ctx.Int("max_confs")),
		})
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var withdrawalCommand = cli.Command{
	Name:      "withdraw",
	ShortName: "w",
	Usage:     "Withdraw from static address deposits.",
	Description: `
	Withdraws from all or selected static address deposits by sweeping them 
	back to our lnd wallet.
	`,
	Flags: []cli.Flag{
		cli.StringSliceFlag{
			Name: "utxo",
			Usage: "specify utxos as outpoints(tx:idx) which will" +
				"be withdrawn.",
		},
		cli.BoolFlag{
			Name:  "all",
			Usage: "withdraws all static address deposits.",
		},
	},
	Action: withdraw,
}

func withdraw(ctx *cli.Context) error {
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "withdraw")
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	var (
		isAllSelected  = ctx.IsSet("all")
		isUtxoSelected = ctx.IsSet("utxo")
		outpoints      []*looprpc.OutPoint
		ctxb           = context.Background()
	)

	switch {
	case isAllSelected == isUtxoSelected:
		return errors.New("must select either all or some utxos")

	case isAllSelected:
	case isUtxoSelected:
		utxos := ctx.StringSlice("utxo")
		outpoints, err = utxosToOutpoints(utxos)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown withdrawal request")
	}

	resp, err := client.WithdrawDeposits(ctxb,
		&looprpc.WithdrawDepositsRequest{
			Outpoints: outpoints,
			All:       isAllSelected,
		})
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var listDepositsCommand = cli.Command{
	Name:  "listdeposits",
	Usage: "Display a summary of static address related information.",
	Description: `
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "filter",
			Usage: "specify a filter to only display deposits in " +
				"the specified state. Leaving out the filter " +
				"returns all deposits.\nThe state can be one " +
				"of the following: \n" +
				"deposited\nwithdrawing\nwithdrawn\n" +
				"looping_in\nlooped_in\n" +
				"publish_expired_deposit\n" +
				"sweep_htlc_timeout\nhtlc_timeout_swept\n" +
				"wait_for_expiry_sweep\nexpired\nfailed\n.",
		},
	},
	Action: listDeposits,
}

var listStaticAddressSwapsCommand = cli.Command{
	Name:  "listswaps",
	Usage: "Display a summary of static address related information.",
	Description: `
	`,
	Action: listStaticAddressSwaps,
}

var summaryCommand = cli.Command{
	Name:      "summary",
	ShortName: "s",
	Usage:     "Display a summary of static address related information.",
	Description: `
	Displays various static address related information like deposits, 
	withdrawals and statistics. The information can be filtered by state.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "filter",
			Usage: "specify a filter to only display deposits in " +
				"the specified state. Leaving out the filter " +
				"returns all deposits.\nThe state can be one " +
				"of the following: \n" +
				"deposited\nwithdrawing\nwithdrawn\n" +
				"looping_in\nlooped_in\n" +
				"publish_expired_deposit\n" +
				"sweep_htlc_timeout\nhtlc_timeout_swept\n" +
				"wait_for_expiry_sweep\nexpired\nfailed\n.",
		},
	},
	Action: summary,
}

func listDeposits(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "listdeposits")
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	var filterState looprpc.DepositState
	switch ctx.String("filter") {
	case "":
		// If no filter is specified, we'll default to showing all.

	case "deposited":
		filterState = looprpc.DepositState_DEPOSITED

	case "withdrawing":
		filterState = looprpc.DepositState_WITHDRAWING

	case "withdrawn":
		filterState = looprpc.DepositState_WITHDRAWN

	case "looping_in":
		filterState = looprpc.DepositState_LOOPING_IN

	case "looped_in":
		filterState = looprpc.DepositState_LOOPED_IN

	case "publish_expired_deposit":
		filterState = looprpc.DepositState_PUBLISH_EXPIRED

	case "sweep_htlc_timeout":
		filterState = looprpc.DepositState_SWEEP_HTLC_TIMEOUT

	case "htlc_timeout_swept":
		filterState = looprpc.DepositState_HTLC_TIMEOUT_SWEPT

	case "wait_for_expiry_sweep":
		filterState = looprpc.DepositState_WAIT_FOR_EXPIRY_SWEEP

	case "expired":
		filterState = looprpc.DepositState_EXPIRED

	default:
		filterState = looprpc.DepositState_UNKNOWN_STATE
	}

	resp, err := client.ListStaticAddressDeposits(
		ctxb, &looprpc.ListStaticAddressDepositsRequest{
			StateFilter: filterState,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

func listStaticAddressSwaps(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "listswaps")
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListStaticAddressSwaps(
		ctxb, &looprpc.ListStaticAddressSwapsRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

func summary(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "summary")
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.GetStaticAddressSummary(
		ctxb, &looprpc.StaticAddressSummaryRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

func utxosToOutpoints(utxos []string) ([]*looprpc.OutPoint, error) {
	outpoints := make([]*looprpc.OutPoint, 0, len(utxos))
	if len(utxos) == 0 {
		return nil, fmt.Errorf("no utxos specified")
	}
	for _, utxo := range utxos {
		outpoint, err := NewProtoOutPoint(utxo)
		if err != nil {
			return nil, err
		}
		outpoints = append(outpoints, outpoint)
	}

	return outpoints, nil
}

// NewProtoOutPoint parses an OutPoint into its corresponding lnrpc.OutPoint
// type.
func NewProtoOutPoint(op string) (*looprpc.OutPoint, error) {
	parts := strings.Split(op, ":")
	if len(parts) != 2 {
		return nil, errors.New("outpoint should be of the form " +
			"txid:index")
	}
	txid := parts[0]
	if hex.DecodedLen(len(txid)) != chainhash.HashSize {
		return nil, fmt.Errorf("invalid hex-encoded txid %v", txid)
	}
	outputIndex, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid output index: %v", err)
	}

	return &looprpc.OutPoint{
		TxidStr:     txid,
		OutputIndex: uint32(outputIndex),
	}, nil
}

func staticAddressLoopIn(ctx *cli.Context) error {
	if ctx.NumFlags() == 0 && ctx.NArg() == 0 {
		return cli.ShowAppHelp(ctx)
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	var (
		ctxb                  = context.Background()
		isAllSelected         = ctx.IsSet("all")
		isUtxoSelected        = ctx.IsSet("utxo")
		label                 = ctx.String("static-loop-in")
		hints                 []*swapserverrpc.RouteHint
		lastHop               []byte
		paymentTimeoutSeconds = uint32(loopin.DefaultPaymentTimeoutSeconds)
	)

	// Validate our label early so that we can fail before getting a quote.
	if err := labels.Validate(label); err != nil {
		return err
	}

	// Private and route hints are mutually exclusive as setting private
	// means we retrieve our own route hints from the connected node.
	hints, err = validateRouteHints(ctx)
	if err != nil {
		return err
	}

	if ctx.IsSet(lastHopFlag.Name) {
		lastHopVertex, err := route.NewVertexFromStr(
			ctx.String(lastHopFlag.Name),
		)
		if err != nil {
			return err
		}

		lastHop = lastHopVertex[:]
	}

	// Get the amount we need to quote for.
	depositList, err := client.ListStaticAddressDeposits(
		ctxb, &looprpc.ListStaticAddressDepositsRequest{
			StateFilter: looprpc.DepositState_DEPOSITED,
		},
	)
	if err != nil {
		return err
	}

	if len(depositList.FilteredDeposits) == 0 {
		errString := fmt.Sprintf("no confirmed deposits available, "+
			"deposits need at least %v confirmations",
			deposit.DefaultConfTarget)

		return errors.New(errString)
	}

	var depositOutpoints []string
	switch {
	case isAllSelected == isUtxoSelected:
		return errors.New("must select either all or some utxos")

	case isAllSelected:
		depositOutpoints = depositsToOutpoints(
			depositList.FilteredDeposits,
		)

	case isUtxoSelected:
		depositOutpoints = ctx.StringSlice("utxo")

	default:
		return fmt.Errorf("unknown quote request")
	}

	if containsDuplicates(depositOutpoints) {
		return errors.New("duplicate outpoints detected")
	}

	quoteReq := &looprpc.QuoteRequest{
		LoopInRouteHints: hints,
		LoopInLastHop:    lastHop,
		Private:          ctx.Bool(privateFlag.Name),
		DepositOutpoints: depositOutpoints,
	}
	quote, err := client.GetLoopInQuote(ctxb, quoteReq)
	if err != nil {
		return err
	}

	limits := getInLimits(quote)

	// populate the quote request with the sum of selected deposits and
	// prompt the user for acceptance.
	quoteReq.Amt, err = sumDeposits(
		depositOutpoints, depositList.FilteredDeposits,
	)
	if err != nil {
		return err
	}

	if !(ctx.Bool("force") || ctx.Bool("f")) {
		err = displayInDetails(quoteReq, quote, ctx.Bool("verbose"))
		if err != nil {
			return err
		}
	}

	if ctx.IsSet("payment_timeout") {
		paymentTimeoutSeconds = uint32(ctx.Duration("payment_timeout").Seconds())
	}

	req := &looprpc.StaticAddressLoopInRequest{
		Outpoints:             depositOutpoints,
		MaxSwapFeeSatoshis:    int64(limits.maxSwapFee),
		LastHop:               lastHop,
		Label:                 ctx.String(labelFlag.Name),
		Initiator:             defaultInitiator,
		RouteHints:            hints,
		Private:               ctx.Bool("private"),
		PaymentTimeoutSeconds: paymentTimeoutSeconds,
	}

	resp, err := client.StaticAddressLoopIn(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

func containsDuplicates(outpoints []string) bool {
	found := make(map[string]struct{})
	for _, outpoint := range outpoints {
		if _, ok := found[outpoint]; ok {
			return true
		}
		found[outpoint] = struct{}{}
	}

	return false
}

func sumDeposits(outpoints []string, deposits []*looprpc.Deposit) (int64,
	error) {

	var sum int64
	depositMap := make(map[string]*looprpc.Deposit)
	for _, deposit := range deposits {
		depositMap[deposit.Outpoint] = deposit
	}

	for _, outpoint := range outpoints {
		if _, ok := depositMap[outpoint]; !ok {
			return 0, fmt.Errorf("deposit %v not found", outpoint)
		}

		sum += depositMap[outpoint].Value
	}

	return sum, nil
}

func depositsToOutpoints(deposits []*looprpc.Deposit) []string {
	outpoints := make([]string, 0, len(deposits))
	for _, deposit := range deposits {
		outpoints = append(outpoints, deposit.Outpoint)
	}

	return outpoints
}
