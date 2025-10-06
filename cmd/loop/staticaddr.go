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
	"github.com/urfave/cli/v3"
)

func init() {
	commands = append(commands, staticAddressCommands)
}

var staticAddressCommands = &cli.Command{
	Name:    "static",
	Aliases: []string{"s"},
	Usage:   "perform on-chain to off-chain swaps using static addresses.",
	Commands: []*cli.Command{
		newStaticAddressCommand,
		listUnspentCommand,
		listDepositsCommand,
		listWithdrawalsCommand,
		listStaticAddressSwapsCommand,
		withdrawalCommand,
		summaryCommand,
		staticAddressLoopInCommand,
	},
}

var newStaticAddressCommand = &cli.Command{
	Name:    "new",
	Aliases: []string{"n"},
	Usage:   "Create a new static loop in address.",
	Description: `
	Requests a new static loop in address from the server. Funds that are
	sent to this address will be locked by a 2:2 multisig between us and the
	loop server, or a timeout path that we can sweep once it opens up. The 
	funds can either be cooperatively spent with a signature from the server
	or looped in.
	`,
	Action: newStaticAddress,
}

func newStaticAddress(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	err := displayNewAddressWarning()
	if err != nil {
		return err
	}

	client, cleanup, err := getClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.NewStaticAddress(
		ctx, &looprpc.NewStaticAddressRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var listUnspentCommand = &cli.Command{
	Name:    "listunspent",
	Aliases: []string{"l"},
	Usage:   "List unspent static address outputs.",
	Description: `
	List all unspent static address outputs. 
	`,
	Flags: []cli.Flag{
		&cli.IntFlag{
			Name: "min_confs",
			Usage: "The minimum amount of confirmations an " +
				"output should have to be listed.",
		},
		&cli.IntFlag{
			Name: "max_confs",
			Usage: "The maximum number of confirmations an " +
				"output could have to be listed.",
		},
	},
	Action: listUnspent,
}

func listUnspent(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListUnspentDeposits(
		ctx, &looprpc.ListUnspentDepositsRequest{
			MinConfs: int32(cmd.Int("min_confs")),
			MaxConfs: int32(cmd.Int("max_confs")),
		})
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var withdrawalCommand = &cli.Command{
	Name:    "withdraw",
	Aliases: []string{"w"},
	Usage:   "Withdraw from static address deposits.",
	Description: `
	Withdraws from all or selected static address deposits by sweeping them
	to the internal wallet or an external address.
	`,
	Flags: []cli.Flag{
		&cli.StringSliceFlag{
			Name: "utxo",
			Usage: "specify utxos as outpoints(tx:idx) which will" +
				"be withdrawn.",
		},
		&cli.BoolFlag{
			Name:  "all",
			Usage: "withdraws all static address deposits.",
		},
		&cli.StringFlag{
			Name: "dest_addr",
			Usage: "the optional address that the withdrawn " +
				"funds should be sent to, if let blank the " +
				"funds will go to lnd's wallet",
		},
		&cli.Int64Flag{
			Name: "sat_per_vbyte",
			Usage: "(optional) a manual fee expressed in " +
				"sat/vbyte that should be used when crafting " +
				"the transaction",
		},
		&cli.IntFlag{
			Name: "amount",
			Usage: "the number of satoshis that should be " +
				"withdrawn from the selected deposits. The " +
				"change is sent back to the static address",
		},
	},
	Action: withdraw,
}

func withdraw(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	var (
		isAllSelected  = cmd.IsSet("all")
		isUtxoSelected = cmd.IsSet("utxo")
		outpoints      []*looprpc.OutPoint
		destAddr       string
	)

	switch {
	case isAllSelected == isUtxoSelected:
		return errors.New("must select either all or some utxos")

	case isAllSelected:
	case isUtxoSelected:
		utxos := cmd.StringSlice("utxo")
		outpoints, err = utxosToOutpoints(utxos)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown withdrawal request")
	}

	if cmd.IsSet("dest_addr") {
		destAddr = cmd.String("dest_addr")
	}

	resp, err := client.WithdrawDeposits(ctx,
		&looprpc.WithdrawDepositsRequest{
			Outpoints:   outpoints,
			All:         isAllSelected,
			DestAddr:    destAddr,
			SatPerVbyte: int64(cmd.Uint64("sat_per_vbyte")),
			Amount:      cmd.Int64("amount"),
		})
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var listDepositsCommand = &cli.Command{
	Name: "listdeposits",
	Usage: "Displays static address deposits. A filter can be applied to " +
		"only show deposits in a specific state.",
	Description: `
	`,
	Flags: []cli.Flag{
		&cli.StringFlag{
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

func listDeposits(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	var filterState looprpc.DepositState
	switch cmd.String("filter") {
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
		ctx, &looprpc.ListStaticAddressDepositsRequest{
			StateFilter: filterState,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var listWithdrawalsCommand = &cli.Command{
	Name:  "listwithdrawals",
	Usage: "Display a summary of past withdrawals.",
	Description: `
	`,
	Action: listWithdrawals,
}

func listWithdrawals(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListStaticAddressWithdrawals(
		ctx, &looprpc.ListStaticAddressWithdrawalRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var listStaticAddressSwapsCommand = &cli.Command{
	Name:  "listswaps",
	Usage: "Shows a list of finalized static address swaps.",
	Description: `
	`,
	Action: listStaticAddressSwaps,
}

func listStaticAddressSwaps(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListStaticAddressSwaps(
		ctx, &looprpc.ListStaticAddressSwapsRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var summaryCommand = &cli.Command{
	Name:    "summary",
	Aliases: []string{"s"},
	Usage:   "Display a summary of static address related information.",
	Description: `
	Displays various static address related information about deposits, 
	withdrawals and swaps. 
	`,
	Action: summary,
}

func summary(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.GetStaticAddressSummary(
		ctx, &looprpc.StaticAddressSummaryRequest{},
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

var staticAddressLoopInCommand = &cli.Command{
	Name:      "in",
	Usage:     "Loop in funds from static address deposits.",
	ArgsUsage: "[amt] [--all | --utxo xxx:xx]",
	Description: `
	Requests a loop-in swap based on static address deposits. After the
	creation of a static address funds can be sent to it. Once the funds are
	confirmed on-chain they can be swapped instantaneously. If deposited
	funds are not needed they can we withdrawn back to the local lnd wallet.
	`,
	Flags: []cli.Flag{
		&cli.StringSliceFlag{
			Name: "utxo",
			Usage: "specify the utxos of deposits as " +
				"outpoints(tx:idx) that should be looped in.",
		},
		&cli.BoolFlag{
			Name:  "all",
			Usage: "loop in all static address deposits.",
		},
		&cli.DurationFlag{
			Name: "payment_timeout",
			Usage: "the maximum time in seconds that the server " +
				"is allowed to take for the swap payment. " +
				"The client can retry the swap with adjusted " +
				"parameters after the payment timed out.",
		},
		&cli.Uint64Flag{
			Name: "amount",
			Usage: "the number of satoshis that should be " +
				"swapped from the selected deposits. If there" +
				"is change it is sent back to the static " +
				"address.",
		},
		&cli.BoolFlag{
			Name: "fast",
			Usage: "Usage: complete the swap faster by paying a " +
				"higher fee, so the change output is " +
				"available sooner",
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

func staticAddressLoopIn(ctx context.Context, cmd *cli.Command) error {
	if cmd.NumFlags() == 0 && cmd.NArg() == 0 {
		return showCommandHelp(ctx, cmd)
	}

	var selectedAmount int64
	switch {
	case cmd.NArg() == 1:
		amt, err := parseAmt(cmd.Args().Get(0))
		if err != nil {
			return err
		}
		selectedAmount = int64(amt)

	case cmd.NArg() > 1:
		return fmt.Errorf("only a single positional argument is " +
			"allowed")

	default:
		selectedAmount = cmd.Int64("amount")
	}

	client, cleanup, err := getClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	var (
		isAllSelected              = cmd.IsSet("all")
		isUtxoSelected             = cmd.IsSet("utxo")
		autoSelectDepositsForQuote bool
		label                      = cmd.String(labelFlag.Name)
		hints                      []*swapserverrpc.RouteHint
		lastHop                    []byte
		paymentTimeoutSeconds      = uint32(loopin.DefaultPaymentTimeoutSeconds)
	)

	// Validate our label early so that we can fail before getting a quote.
	if err := labels.Validate(label); err != nil {
		return err
	}

	// Private and route hints are mutually exclusive as setting private
	// means we retrieve our own route hints from the connected node.
	hints, err = validateRouteHints(cmd)
	if err != nil {
		return err
	}

	if cmd.IsSet(lastHopFlag.Name) {
		lastHopVertex, err := route.NewVertexFromStr(
			cmd.String(lastHopFlag.Name),
		)
		if err != nil {
			return err
		}

		lastHop = lastHopVertex[:]
	}

	// Get the amount we need to quote for.
	depositList, err := client.ListStaticAddressDeposits(
		ctx, &looprpc.ListStaticAddressDepositsRequest{
			StateFilter: looprpc.DepositState_DEPOSITED,
		},
	)
	if err != nil {
		return err
	}

	allDeposits := depositList.FilteredDeposits

	if len(allDeposits) == 0 {
		errString := fmt.Sprintf("no confirmed deposits available, "+
			"deposits need at least %v confirmations",
			deposit.MinConfs)

		return errors.New(errString)
	}

	var depositOutpoints []string
	switch {
	case isAllSelected && isUtxoSelected:
		return errors.New("cannot select all and specific utxos")

	case isAllSelected:
		depositOutpoints = depositsToOutpoints(allDeposits)

	case isUtxoSelected:
		depositOutpoints = cmd.StringSlice("utxo")

	case selectedAmount > 0:
		// If only an amount is selected, we will trigger coin
		// selection.

	default:
		return fmt.Errorf("unknown quote request")
	}

	if containsDuplicates(depositOutpoints) {
		return errors.New("duplicate outpoints detected")
	}

	if len(depositOutpoints) == 0 && selectedAmount > 0 {
		autoSelectDepositsForQuote = true
	}

	quoteReq := &looprpc.QuoteRequest{
		Amt:                selectedAmount,
		LoopInRouteHints:   hints,
		LoopInLastHop:      lastHop,
		Private:            cmd.Bool(privateFlag.Name),
		DepositOutpoints:   depositOutpoints,
		AutoSelectDeposits: autoSelectDepositsForQuote,
		Fast:               cmd.Bool("fast"),
	}
	quote, err := client.GetLoopInQuote(ctx, quoteReq)
	if err != nil {
		return err
	}

	limits := getInLimits(quote)

	if !(cmd.Bool("force") || cmd.Bool("f")) {
		err = displayInDetails(quoteReq, quote, cmd.Bool("verbose"))
		if err != nil {
			return err
		}
	}

	if cmd.IsSet("payment_timeout") {
		paymentTimeoutSeconds = uint32(cmd.Duration("payment_timeout").Seconds())
	}

	req := &looprpc.StaticAddressLoopInRequest{
		Amount:                quoteReq.Amt,
		Outpoints:             depositOutpoints,
		MaxSwapFeeSatoshis:    int64(limits.maxSwapFee),
		LastHop:               lastHop,
		Label:                 label,
		Initiator:             defaultInitiator,
		RouteHints:            hints,
		Private:               cmd.Bool("private"),
		PaymentTimeoutSeconds: paymentTimeoutSeconds,
		Fast:                  cmd.Bool("fast"),
	}

	resp, err := client.StaticAddressLoopIn(ctx, req)
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

func depositsToOutpoints(deposits []*looprpc.Deposit) []string {
	outpoints := make([]string, 0, len(deposits))
	for _, deposit := range deposits {
		outpoints = append(outpoints, deposit.Outpoint)
	}

	return outpoints
}

func displayNewAddressWarning() error {
	fmt.Printf("\nWARNING: Be aware that loosing your l402.token file in " +
		".loop under your home directory will take your ability to " +
		"spend funds sent to the static address via loop-ins or " +
		"withdrawals. You will have to wait until the deposit " +
		"expires and your loop client sweeps the funds back to your " +
		"lnd wallet. The deposit expiry could be months in the " +
		"future.\n")

	fmt.Printf("\nCONTINUE WITH NEW ADDRESS? (y/n): ")

	var answer string
	fmt.Scanln(&answer)
	if answer == "y" {
		return nil
	}

	return errors.New("new address creation canceled")
}
