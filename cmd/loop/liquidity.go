package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
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
	Usage:       "set liquidity manager rule for a channel",
	Description: "Update or remove the liquidity rule for a channel.",
	ArgsUsage:   "shortchanid",
	Flags: []cli.Flag{
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
			Name:  "clear",
			Usage: "remove the rule currently set for the channel.",
		},
	},
	Action: setRule,
}

func setRule(ctx *cli.Context) error {
	// We require that a channel ID is set for this rule update.
	if ctx.NArg() != 1 {
		return fmt.Errorf("please set a channel id for the rule " +
			"update")
	}

	chanID, err := strconv.ParseUint(ctx.Args().First(), 10, 64)
	if err != nil {
		return fmt.Errorf("could not parse channel ID: %v", err)
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
	// currently set for this channel. We also track a slice containing all
	// of the rules we currently have set for other channels, because we
	// want to leave these rules untouched.
	for _, rule := range params.Rules {
		if rule.ChannelId == chanID {
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
	Name:        "setparams",
	Usage:       "update the parameters set for the liquidity manager",
	Description: "Updates the parameters set for the liquidity manager.",
	Flags: []cli.Flag{
		cli.IntFlag{
			Name: "sweeplimit",
			Usage: "the limit placed on our estimated sweep fee " +
				"in sat/vByte.",
		},
		cli.Float64Flag{
			Name: "maxswapfee",
			Usage: "the maximum percentage of swap volume we are " +
				"willing to pay in server fees.",
		},
		cli.Float64Flag{
			Name: "maxroutingfee",
			Usage: "the maximum percentage of off-chain payment " +
				"volume that are are willing to pay in " +
				"routing fees.",
		},
		cli.Float64Flag{
			Name: "maxprepayfee",
			Usage: "the maximum percentage of off-chain prepay " +
				"volume that are are willing to pay in " +
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

	var flagSet bool

	if ctx.IsSet("maxswapfee") {
		feeRate := ctx.Float64("maxswapfee")
		params.MaxSwapFeePpm, err = ppmFromPercentage(feeRate)
		if err != nil {
			return err
		}

		flagSet = true
	}

	if ctx.IsSet("sweeplimit") {
		satPerVByte := ctx.Int("sweeplimit")
		params.SweepFeeRateSatPerVbyte = uint64(satPerVByte)

		flagSet = true
	}

	if ctx.IsSet("maxroutingfee") {
		feeRate := ctx.Float64("maxroutingfee")
		params.MaxRoutingFeePpm, err = ppmFromPercentage(feeRate)
		if err != nil {
			return err
		}

		flagSet = true
	}

	if ctx.IsSet("maxprepayfee") {
		feeRate := ctx.Float64("maxprepayfee")
		params.MaxPrepayRoutingFeePpm, err = ppmFromPercentage(feeRate)
		if err != nil {
			return err
		}

		flagSet = true
	}

	if ctx.IsSet("maxprepay") {
		params.MaxPrepaySat = ctx.Uint64("maxprepay")
		flagSet = true
	}

	if ctx.IsSet("maxminer") {
		params.MaxMinerFeeSat = ctx.Uint64("maxminer")
		flagSet = true
	}

	if ctx.IsSet("sweepconf") {
		params.SweepConfTarget = int32(ctx.Int("sweepconf"))
		flagSet = true
	}

	if ctx.IsSet("failurebackoff") {
		params.FailureBackoffSec = ctx.Uint64("failurebackoff")
		flagSet = true
	}

	if !flagSet {
		return fmt.Errorf("at least one flag required to set params")
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
	if err != nil {
		return err
	}

	printJSON(resp)

	return nil
}
