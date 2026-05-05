package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
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
		openChannelCommand,
	},
}

var newStaticAddressCommand = &cli.Command{
	Name:    "new",
	Aliases: []string{"n"},
	Usage:   "Return the static loop in address.",
	Description: `
	Returns the current static loop in address. On a fresh installation loopd
	initializes the current static-address generation during startup. If the
	address is still missing, this call will create it on demand. Funds sent
	to the address will be locked by a 2:2 multisig between us and the loop
	server, or a timeout path that we can sweep once it opens up. The funds
	can either be cooperatively spent with a signature from the server or
	looped in.
	`,
	Action: newStaticAddress,
}

func newStaticAddress(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	err = maybeDisplayNewAddressWarning(ctx, client)
	if err != nil {
		return err
	}

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

	client, cleanup, err := getClient(cmd)
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
		&cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: "(optional) a manual fee expressed in " +
				"sat/vbyte that should be used when crafting " +
				"the transaction",
		},
		&cli.Uint64Flag{
			Name:    "amt",
			Aliases: []string{"amount"},
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

	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	var (
		isAllSelected  = cmd.IsSet("all")
		isUtxoSelected = cmd.IsSet("utxo")
		outpoints      []*lnrpc.OutPoint
		destAddr       string
	)

	switch {
	case isAllSelected == isUtxoSelected:
		return errors.New("must select either all or some utxos")

	case isAllSelected:
	case isUtxoSelected:
		utxos := cmd.StringSlice("utxo")
		outpoints, err = lnd.UtxosToOutpoints(utxos)
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
			Amount:      int64(cmd.Uint64("amt")),
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
				"opening_channel\nchannel_published\n" +
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

	client, cleanup, err := getClient(cmd)
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

	case "opening_channel":
		filterState = looprpc.DepositState_OPENING_CHANNEL

	case "channel_published":
		filterState = looprpc.DepositState_CHANNEL_PUBLISHED

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

	client, cleanup, err := getClient(cmd)
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
	Usage: "Shows a list of static address swaps.",
	Description: `
	`,
	Action: listStaticAddressSwaps,
}

func listStaticAddressSwaps(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(cmd)
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
	withdrawals, swaps and channel openings. 
	`,
	Action: summary,
}

func summary(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(cmd)
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
			Name:    "amt",
			Aliases: []string{"amount"},
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
		&cli.Uint64Flag{
			Name: "max_swap_fee_sat",
			Usage: fmt.Sprintf("the maximum swap fee in satoshis. "+
				"If set, the swap is rejected when the quoted "+
				"fee exceeds this cap. The maximum allowed "+
				"value is %d. On-chain fees for creating "+
				"static deposits are unaffected.",
				maxSwapFeeSatLimit),
		},
		&cli.Uint64Flag{
			Name: "max_swap_fee_ppm",
			Usage: "the maximum swap fee expressed in " +
				"parts per million of the swap amount. " +
				"If set together with --max_swap_fee_sat " +
				"the tighter cap is used.",
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
		selectedAmount = int64(cmd.Uint64("amt"))
	}

	client, cleanup, err := getClient(cmd)
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
		return errors.New("no deposited outputs available")
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

	// Resolve the effective swap fee cap. When the user provides
	// --max_swap_fee_sat and/or --max_swap_fee_ppm the tighter of
	// the two is used and checked against the current quote. Without
	// overrides the quoted fee is forwarded as before.
	maxSwapFee, err := resolveMaxSwapFee(
		quoteReq, quote,
		cmd.IsSet("max_swap_fee_sat"), cmd.Uint64("max_swap_fee_sat"),
		cmd.IsSet("max_swap_fee_ppm"), cmd.Uint64("max_swap_fee_ppm"),
	)
	if err != nil {
		return err
	}

	// Warn the user if any selected deposits have fewer than 6
	// confirmations, as the swap payment won't be received immediately
	// for those.
	summary, err := client.GetStaticAddressSummary(
		ctx, &looprpc.StaticAddressSummaryRequest{},
	)
	if err != nil {
		return err
	}

	depositsToCheck := warningDepositOutpoints(
		allDeposits, depositOutpoints, autoSelectDepositsForQuote,
		quoteReq.Amt,
	)
	warning := lowConfDepositWarning(
		allDeposits, depositsToCheck,
		int64(summary.RelativeExpiryBlocks),
	)
	if warning != "" {
		fmt.Println(warning)
	}

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
		MaxSwapFeeSatoshis:    int64(maxSwapFee),
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

var warningSelectionDustLimit = int64(lnwallet.DustLimitForSize(input.P2TRSize))

// warningDepositOutpoints returns the deposit outpoints to check for
// low-confirmation warnings.
func warningDepositOutpoints(allDeposits []*looprpc.Deposit,
	selectedOutpoints []string, autoSelect bool, targetAmount int64) []string {

	if !autoSelect {
		return selectedOutpoints
	}

	return autoSelectedWarningOutpoints(allDeposits, targetAmount)
}

// autoSelectedWarningOutpoints returns the outpoints selected by the same
// ordering used for automatic static loop-in deposit selection.
func autoSelectedWarningOutpoints(allDeposits []*looprpc.Deposit,
	targetAmount int64) []string {

	if targetAmount <= 0 {
		return nil
	}

	// KEEP IN SYNC with staticaddr/loopin.SelectDeposits.
	deposits := filterSwappableWarningDeposits(allDeposits)
	sort.Slice(deposits, func(i, j int) bool {
		iConfirmed := deposits[i].ConfirmationHeight > 0
		jConfirmed := deposits[j].ConfirmationHeight > 0
		if iConfirmed != jConfirmed {
			return iConfirmed
		}

		if deposits[i].Value == deposits[j].Value {
			return deposits[i].BlocksUntilExpiry <
				deposits[j].BlocksUntilExpiry
		}

		return deposits[i].Value > deposits[j].Value
	})

	selectedOutpoints := make([]string, 0, len(deposits))
	var selectedAmount int64
	for _, deposit := range deposits {
		selectedOutpoints = append(selectedOutpoints, deposit.Outpoint)
		selectedAmount += deposit.Value
		if selectedAmount == targetAmount {
			return selectedOutpoints
		}

		if selectedAmount > targetAmount &&
			selectedAmount-targetAmount >= warningSelectionDustLimit {

			return selectedOutpoints
		}
	}

	return nil
}

// filterSwappableWarningDeposits filters deposits for CLI warning selection.
func filterSwappableWarningDeposits(
	allDeposits []*looprpc.Deposit) []*looprpc.Deposit {

	swappable := make([]*looprpc.Deposit, 0, len(allDeposits))
	minBlocksUntilExpiry := int64(
		loopin.DefaultLoopInOnChainCltvDelta + loopin.DepositHtlcDelta,
	)
	for _, deposit := range allDeposits {
		// Unconfirmed deposits remain swappable because their CSV timeout has
		// not started yet. This mirrors loopin.IsSwappable.
		if deposit.ConfirmationHeight > 0 &&
			deposit.BlocksUntilExpiry < minBlocksUntilExpiry {

			continue
		}

		swappable = append(swappable, deposit)
	}

	return swappable
}

// conservativeWarningConfs is the highest default confirmation tier used by
// the server's dynamic confirmation-risk policy.
//
// The CLI does not currently know the server's exact policy, so we use this
// conservative threshold for warnings without promising immediate execution.
const conservativeWarningConfs = 6

// lowConfDepositWarning checks the selected deposits against a conservative
// confirmation threshold and returns a warning string if any are found.
func lowConfDepositWarning(allDeposits []*looprpc.Deposit,
	selectedOutpoints []string, csvExpiry int64) string {

	depositMap := make(map[string]*looprpc.Deposit, len(allDeposits))
	for _, d := range allDeposits {
		depositMap[d.Outpoint] = d
	}

	var lowConfEntries []string
	for _, op := range selectedOutpoints {
		d, ok := depositMap[op]
		if !ok {
			continue
		}

		var confs int64
		switch {
		case d.ConfirmationHeight <= 0:
			confs = 0

		case csvExpiry > 0:
			// For confirmed deposits we can compute
			// confirmations as CSVExpiry - BlocksUntilExpiry + 1.
			confs = csvExpiry - d.BlocksUntilExpiry + 1

		default:
			// Can't determine confirmations without the CSV expiry.
			continue
		}

		if confs >= conservativeWarningConfs {
			continue
		}

		if confs == 0 {
			lowConfEntries = append(
				lowConfEntries,
				fmt.Sprintf("  - %s (unconfirmed)", op),
			)
		} else {
			lowConfEntries = append(
				lowConfEntries,
				fmt.Sprintf(
					"  - %s (%d confirmations)", op,
					confs,
				),
			)
		}
	}

	if len(lowConfEntries) == 0 {
		return ""
	}

	return fmt.Sprintf(
		"\nWARNING: The following deposits are below the "+
			"conservative %d-confirmation threshold:\n%s\n"+
			"The swap payment for these deposits may wait for "+
			"more confirmations depending on the server's "+
			"confirmation-risk policy.\n",
		conservativeWarningConfs,
		strings.Join(lowConfEntries, "\n"),
	)
}

func maybeDisplayNewAddressWarning(ctx context.Context,
	client looprpc.SwapClientClient) error {

	_, err := client.GetStaticAddressSummary(
		ctx, &looprpc.StaticAddressSummaryRequest{},
	)
	switch {
	case err == nil:
		return nil

	case strings.Contains(err.Error(), address.ErrNoStaticAddress.Error()):
		return displayNewAddressWarning()

	default:
		return nil
	}
}

func displayNewAddressWarning() error {
	fmt.Printf("\nWARNING: Be aware that losing your l402.token file in " +
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
