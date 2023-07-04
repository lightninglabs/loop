package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/urfave/cli"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var getLiquidityParamsCommand = cli.Command{
	Name:  "getparams",
	Usage: "show liquidity manager parameters",
	Description: "Displays the current set of parameters that are set " +
		"for the liquidity manager.",
	Action: getParams,
}

func getParams(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	cfg, err := client.GetLiquidityParams(
		context.Background(), &looprpc.GetLiquidityParamsRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(cfg)

	return nil
}

var setLiquidityRuleCommand = cli.Command{
	Name:        "setrule",
	Usage:       "set liquidity manager rule for a channel/peer",
	Description: "Update or remove the liquidity rule for a channel/peer.",
	ArgsUsage:   "{shortchanid |  peerpubkey}",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "type",
			Usage: "the type of swap to perform, set to 'out' " +
				"for acquiring inbound liquidity or 'in' for " +
				"acquiring outbound liquidity.",
			Value: "out",
		},

		cli.IntFlag{
			Name: "incoming_threshold",
			Usage: "the minimum percentage of incoming liquidity " +
				"to total capacity beneath which to " +
				"recommend loop out to acquire incoming.",
		},
		cli.IntFlag{
			Name: "outgoing_threshold",
			Usage: "the minimum percentage of outbound liquidity " +
				"that we do not want to drop below.",
		},
		cli.BoolFlag{
			Name: "clear",
			Usage: "remove the rule currently set for the " +
				"channel/peer.",
		},
	},
	Action: setRule,
}

func setRule(ctx *cli.Context) error {
	// We require that a channel ID is set for this rule update.
	if ctx.NArg() != 1 {
		return fmt.Errorf("please set a channel id or peer pubkey " +
			"for the rule update")
	}

	var (
		pubkey     route.Vertex
		pubkeyRule bool
	)
	chanID, err := strconv.ParseUint(ctx.Args().First(), 10, 64)
	if err != nil {
		pubkey, err = route.NewVertexFromStr(ctx.Args().First())
		if err != nil {
			return fmt.Errorf("please provide a valid pubkey: "+
				"%v, or short channel ID", err)
		}
		pubkeyRule = true
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// We need to set the full set of current parameters every time we call
	// SetParameters. To allow users to set only individual fields on the
	// cli, we lookup our current params, then update individual values.
	params, err := client.GetLiquidityParams(
		context.Background(), &looprpc.GetLiquidityParamsRequest{},
	)
	if err != nil {
		return err
	}

	var (
		inboundSet  = ctx.IsSet("incoming_threshold")
		outboundSet = ctx.IsSet("outgoing_threshold")
		ruleSet     bool
		otherRules  []*looprpc.LiquidityRule
	)

	// Run through our current set of rules and check whether we have a rule
	// currently set for this channel or peer. We also track a slice
	// containing all of the rules we currently have set for other channels,
	// and peers because we want to leave these rules untouched.
	for _, rule := range params.Rules {
		var (
			channelRuleSet = rule.ChannelId != 0 &&
				rule.ChannelId == chanID

			peerRuleSet = rule.Pubkey != nil && bytes.Equal(
				rule.Pubkey, pubkey[:],
			)
		)

		if channelRuleSet || peerRuleSet {
			ruleSet = true
		} else {
			otherRules = append(otherRules, rule)
		}
	}

	// If we want to clear the rule for this channel, check that we had a
	// rule set in the first place, and set our parameters to the current
	// set excluding the channel specified.
	if ctx.IsSet("clear") {
		if !ruleSet {
			return fmt.Errorf("cannot clear channel: %v, no rule "+
				"set at present", chanID)
		}

		if inboundSet || outboundSet {
			return fmt.Errorf("do not set other flags with clear " +
				"flag")
		}

		params.Rules = otherRules
		_, err = client.SetLiquidityParams(
			context.Background(),
			&looprpc.SetLiquidityParamsRequest{
				Parameters: params,
			},
		)
		return err
	}

	// If we are setting a rule for this channel (not clearing it), check
	// that at least one value is set.
	if !inboundSet && !outboundSet {
		return fmt.Errorf("provide at least one flag to set rules or " +
			"use the --clear flag to remove rules")
	}

	// Create a new rule which will be used to overwrite our current rule.
	newRule := &looprpc.LiquidityRule{
		ChannelId: chanID,
		Type:      looprpc.LiquidityRuleType_THRESHOLD,
	}
	if ctx.IsSet("type") {
		switch ctx.String("type") {
		case "in":
			newRule.SwapType = looprpc.SwapType_LOOP_IN

		case "out":
			newRule.SwapType = looprpc.SwapType_LOOP_OUT

		default:
			return errors.New("please set type to in or out")
		}
	}

	if pubkeyRule {
		newRule.Pubkey = pubkey[:]
	}

	if inboundSet {
		newRule.IncomingThreshold = uint32(
			ctx.Int("incoming_threshold"),
		)
	}

	if outboundSet {
		newRule.OutgoingThreshold = uint32(
			ctx.Int("outgoing_threshold"),
		)
	}

	// Just set the rules on our current set of parameters and leave the
	// other values untouched.
	otherRules = append(otherRules, newRule)
	params.Rules = otherRules

	// Update our parameters to the existing set, plus our new rule.
	_, err = client.SetLiquidityParams(
		context.Background(),
		&looprpc.SetLiquidityParamsRequest{
			Parameters: params,
		},
	)

	return err
}

var setParamsCommand = cli.Command{
	Name:  "setparams",
	Usage: "update the parameters set for the liquidity manager",
	Description: "Updates the parameters set for the liquidity manager. " +
		"Note the parameters are persisted in db to save the trouble " +
		"of setting them again upon loopd restart. To get the default" +
		"values, use `getparams` before any `setparams`.",
	Flags: []cli.Flag{
		cli.IntFlag{
			Name: "sweeplimit",
			Usage: "the limit placed on our estimated sweep fee " +
				"in sat/vByte.",
		},
		cli.Float64Flag{
			Name: "feepercent",
			Usage: "the maximum percentage of swap amount to be " +
				"used across all fee categories",
		},
		cli.Float64Flag{
			Name: "maxswapfee",
			Usage: "the maximum percentage of swap volume we are " +
				"willing to pay in server fees.",
		},
		cli.Float64Flag{
			Name: "maxroutingfee",
			Usage: "the maximum percentage of off-chain payment " +
				"volume that we are willing to pay in routing" +
				"fees.",
		},
		cli.Float64Flag{
			Name: "maxprepayfee",
			Usage: "the maximum percentage of off-chain prepay " +
				"volume that we are willing to pay in " +
				"routing fees.",
		},
		cli.Uint64Flag{
			Name: "maxprepay",
			Usage: "the maximum no-show (prepay) in satoshis that " +
				"swap suggestions should be limited to.",
		},
		cli.Uint64Flag{
			Name: "maxminer",
			Usage: "the maximum miner fee in satoshis that swap " +
				"suggestions should be limited to.",
		},
		cli.IntFlag{
			Name: "sweepconf",
			Usage: "the number of blocks from htlc height that " +
				"swap suggestion sweeps should target, used " +
				"to estimate max miner fee.",
		},
		cli.Uint64Flag{
			Name: "failurebackoff",
			Usage: "the amount of time, in seconds, that " +
				"should pass before a channel that " +
				"previously had a failed swap will be " +
				"included in suggestions.",
		},
		cli.BoolFlag{
			Name: "autoloop",
			Usage: "set to true to enable automated dispatch " +
				"of swaps, limited to the budget set by " +
				"autobudget.",
		},
		cli.StringFlag{
			Name: "destaddr",
			Usage: "custom address to be used as destination for " +
				"autoloop loop out, set to \"default\" in " +
				"order to revert to default behavior.",
		},
		cli.StringFlag{
			Name: "account",
			Usage: "the name of the account to generate a new " +
				"address from. You can list the names of " +
				"valid accounts in your backing lnd " +
				"instance with \"lncli wallet accounts list\".",
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
			Name: "autobudget",
			Usage: "the maximum amount of fees in satoshis that " +
				"automatically dispatched loop out swaps may " +
				"spend.",
		},
		cli.DurationFlag{
			Name: "autobudgetrefreshperiod",
			Usage: "the time period over which the automated " +
				"loop budget is refreshed.",
		},
		cli.Uint64Flag{
			Name: "autoinflight",
			Usage: "the maximum number of automatically " +
				"dispatched swaps that we allow to be in " +
				"flight.",
		},
		cli.Uint64Flag{
			Name: "minamt",
			Usage: "the minimum amount in satoshis that the " +
				"autoloop client will dispatch per-swap.",
		},
		cli.Uint64Flag{
			Name: "maxamt",
			Usage: "the maximum amount in satoshis that the " +
				"autoloop client will dispatch per-swap.",
		},
		cli.IntFlag{
			Name: "htlc_conf",
			Usage: "the confirmation target for loop in on-chain " +
				"htlcs.",
		},
		cli.BoolFlag{
			Name: "easyautoloop",
			Usage: "set to true to enable easy autoloop, which " +
				"will automatically dispatch swaps in order " +
				"to meet the target local balance.",
		},
		cli.Uint64Flag{
			Name: "localbalancesat",
			Usage: "the target size of total local balance in " +
				"satoshis, used by easy autoloop.",
		},
	},
	Action: setParams,
}

func setParams(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// We need to set the full set of current parameters every time we call
	// SetParameters. To allow users to set only individual fields on the
	// cli, we lookup our current params, then update individual values.
	params, err := client.GetLiquidityParams(
		context.Background(), &looprpc.GetLiquidityParamsRequest{},
	)
	if err != nil {
		return err
	}

	var flagSet, categoriesSet, feePercentSet bool

	// Update our existing parameters with the values provided by cli flags.
	// Our fee categories and fee percentage are exclusive, so track which
	// flags are set to ensure that we don't have nonsensical overlap.
	if ctx.IsSet("maxswapfee") {
		feeRate := ctx.Float64("maxswapfee")
		params.MaxSwapFeePpm, err = ppmFromPercentage(feeRate)
		if err != nil {
			return err
		}

		flagSet = true
		categoriesSet = true
	}

	if ctx.IsSet("sweeplimit") {
		satPerVByte := ctx.Int("sweeplimit")
		params.SweepFeeRateSatPerVbyte = uint64(satPerVByte)

		flagSet = true
		categoriesSet = true
	}

	if ctx.IsSet("feepercent") {
		feeRate := ctx.Float64("feepercent")
		params.FeePpm, err = ppmFromPercentage(feeRate)
		if err != nil {
			return err
		}

		flagSet = true
		feePercentSet = true
	}

	if ctx.IsSet("maxroutingfee") {
		feeRate := ctx.Float64("maxroutingfee")
		params.MaxRoutingFeePpm, err = ppmFromPercentage(feeRate)
		if err != nil {
			return err
		}

		flagSet = true
		categoriesSet = true
	}

	if ctx.IsSet("maxprepayfee") {
		feeRate := ctx.Float64("maxprepayfee")
		params.MaxPrepayRoutingFeePpm, err = ppmFromPercentage(feeRate)
		if err != nil {
			return err
		}

		flagSet = true
		categoriesSet = true
	}

	if ctx.IsSet("maxprepay") {
		params.MaxPrepaySat = ctx.Uint64("maxprepay")
		flagSet = true
		categoriesSet = true
	}

	if ctx.IsSet("maxminer") {
		params.MaxMinerFeeSat = ctx.Uint64("maxminer")
		flagSet = true
		categoriesSet = true
	}

	if ctx.IsSet("sweepconf") {
		params.SweepConfTarget = int32(ctx.Int("sweepconf"))
		flagSet = true
	}

	if ctx.IsSet("failurebackoff") {
		params.FailureBackoffSec = ctx.Uint64("failurebackoff")
		flagSet = true
	}

	if ctx.IsSet("autoloop") {
		params.Autoloop = ctx.Bool("autoloop")
		flagSet = true
	}

	if ctx.IsSet("autobudget") {
		params.AutoloopBudgetSat = ctx.Uint64("autobudget")
		flagSet = true
	}

	switch {
	case ctx.IsSet("destaddr") && ctx.IsSet("account"):
		return fmt.Errorf("cannot set destaddr and account at the " +
			"same time")

	case ctx.IsSet("destaddr"):
		params.AutoloopDestAddress = ctx.String("destaddr")
		params.Account = ""
		flagSet = true

	case ctx.IsSet("account") != ctx.IsSet("account_addr_type"):
		return liquidity.ErrAccountAndAddrType

	case ctx.IsSet("account"):
		params.Account = ctx.String("account")
		params.AutoloopDestAddress = ""
		flagSet = true
	}

	if ctx.IsSet("account_addr_type") {
		switch ctx.String("account_addr_type") {
		case "p2tr":
			params.AccountAddrType = looprpc.AddressType_TAPROOT_PUBKEY

		default:
			return fmt.Errorf("unknown account address type")
		}
	}

	if ctx.IsSet("autobudgetrefreshperiod") {
		params.AutoloopBudgetRefreshPeriodSec =
			uint64(ctx.Duration("autobudgetrefreshperiod").Seconds())
		flagSet = true
	}

	if ctx.IsSet("autoinflight") {
		params.AutoMaxInFlight = ctx.Uint64("autoinflight")
		flagSet = true
	}

	if ctx.IsSet("minamt") {
		params.MinSwapAmount = ctx.Uint64("minamt")
		flagSet = true
	}

	if ctx.IsSet("maxamt") {
		params.MaxSwapAmount = ctx.Uint64("maxamt")
		flagSet = true
	}

	if ctx.IsSet("htlc_conf") {
		params.HtlcConfTarget = int32(ctx.Int("htlc_conf"))
		flagSet = true
	}

	if ctx.IsSet("easyautoloop") {
		params.EasyAutoloop = ctx.Bool("easyautoloop")
		flagSet = true
	}

	if ctx.IsSet("localbalancesat") {
		params.EasyAutoloopLocalTargetSat =
			ctx.Uint64("localbalancesat")
		flagSet = true
	}

	if !flagSet {
		return fmt.Errorf("at least one flag required to set params")
	}

	switch {
	// Fail if fee params for both types of fee limit are set, since they
	// cannot be used in conjunction.
	case feePercentSet && categoriesSet:
		return fmt.Errorf("feepercent cannot be set with specific " +
			"fee category flags")

	// If we are updating to fee percentage, we unset all other fee related
	// params so that users do not need to manually unset them.
	case feePercentSet:
		params.SweepFeeRateSatPerVbyte = 0
		params.MaxMinerFeeSat = 0
		params.MaxPrepayRoutingFeePpm = 0
		params.MaxPrepaySat = 0
		params.MaxRoutingFeePpm = 0
		params.MaxSwapFeePpm = 0

	// If we are setting any of our fee categories, unset fee percentage
	// so that it does not need to be manually updated.
	case categoriesSet:
		params.FeePpm = 0
	}
	// Update our parameters to our mutated values.
	_, err = client.SetLiquidityParams(
		context.Background(), &looprpc.SetLiquidityParamsRequest{
			Parameters: params,
		},
	)

	return err
}

// ppmFromPercentage converts a percentage, expressed as a float, to parts
// per million.
func ppmFromPercentage(percentage float64) (uint64, error) {
	if percentage <= 0 || percentage >= 100 {
		return 0, fmt.Errorf("fee percentage must be in (0;100)")
	}

	return uint64(percentage / 100 * liquidity.FeeBase), nil
}

var suggestSwapCommand = cli.Command{
	Name:  "suggestswaps",
	Usage: "show a list of suggested swaps",
	Description: "Displays a list of suggested swaps that aim to obtain " +
		"the liquidity balance as specified by the rules set in " +
		"the liquidity manager.",
	Action: suggestSwap,
}

func suggestSwap(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.SuggestSwaps(
		context.Background(), &looprpc.SuggestSwapsRequest{},
	)
	if err == nil {
		printRespJSON(resp)
		return nil
	}

	// If we got an error because no rules are set, we want to display a
	// friendly message.
	rpcErr, ok := status.FromError(err)
	if !ok {
		return err
	}

	if rpcErr.Code() != codes.FailedPrecondition {
		return err
	}

	return errors.New("no rules set for autolooper, please set rules " +
		"using the setrule command")
}
