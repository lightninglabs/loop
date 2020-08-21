package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var getLiquidityCfgCommand = cli.Command{
	Name:  "getcfg",
	Usage: "show liquidity manager parameters",
	Description: "Displays the current set of parameters and rules that " +
		"are set for the liquidity manager subsystem.",
	Action: getCfg,
}

func getCfg(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	cfg, err := client.GetLiquidityConfig(
		context.Background(), &looprpc.GetLiquidityConfigRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(cfg)

	return nil
}

var setLiquidityCfgCommand = cli.Command{
	Name:  "setcfg",
	Usage: "set liquidity manager parameters",
	Description: "Updates the current set of parameters that are set " +
		"for the liquidity manager subsystem.",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "inclprivate",
			Usage: "whether to include private channels in our " +
				"balance calculations.",
		},
	},
	Action: setCfg,
}

func setCfg(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// We need to set all values in the config that we send the server to
	// update us to. To allow users to set only individual fields on the
	// cli, we lookup our current config, then update its values.
	cfg, err := client.GetLiquidityConfig(
		context.Background(), &looprpc.GetLiquidityConfigRequest{},
	)
	if err != nil {
		return err
	}

	// If our config was not set before this, we set it to a non-nil value
	// now.
	if cfg == nil {
		cfg = &looprpc.LiquidityConfig{}
	}

	if ctx.IsSet("inclprivate") {
		cfg.IncludePrivate = ctx.Bool("inclprivate")
	}

	// Create a request to update our config.
	req := &looprpc.SetLiquidityConfigRequest{
		Config: cfg,
	}

	_, err = client.SetLiquidityConfig(context.Background(), req)
	return err
}

var setLiquidityRuleCommand = cli.Command{
	Name:  "setrule",
	Usage: "set liquidity manger rule for a target",
	Description: "Updates the liquidity rule that we set for a target. " +
		"At present rules can be set for your node as a whole, or on " +
		"a per-peer level.",
	ArgsUsage: fmt.Sprintf("[%v|%v|%v|%v]", liquidity.TargetNone,
		liquidity.TargetNode, liquidity.TargetPeer,
		liquidity.TargetChannel),
	Flags: []cli.Flag{
		cli.IntFlag{
			Name: "mininbound",
			Usage: "the minimum percentage of inbound liquidity " +
				"to total capacity beneath which to " +
				"recommend loop out to acquire inbound.",
		},
		cli.IntFlag{
			Name: "minoutbound",
			Usage: "the minimum percentage of outbound liquidity " +
				"to total capacity beneath which to " +
				"recommend loop in to acquire outbound.",
		},
	},
	Action: setRule,
}

func setRule(ctx *cli.Context) error {
	// We require that a target is set for this rule update.
	if ctx.NArg() != 1 {
		return fmt.Errorf("please set a target for the rule " +
			"update")
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// We need to set all values in the config that we send the server to
	// update us to. To allow users to set only individual fields on the
	// cli, we lookup our current config, then update its values.
	cfg, err := client.GetLiquidityConfig(
		context.Background(), &looprpc.GetLiquidityConfigRequest{},
	)
	if err != nil {
		return err
	}

	target := ctx.Args().First()
	switch target {
	case liquidity.TargetNone.String():
		cfg.Target = looprpc.LiquidityTarget_NONE

	case liquidity.TargetNode.String():
		cfg.Target = looprpc.LiquidityTarget_NODE

	case liquidity.TargetPeer.String():
		cfg.Target = looprpc.LiquidityTarget_PEER

	case liquidity.TargetChannel.String():
		cfg.Target = looprpc.LiquidityTarget_CHANNEL

	default:
		return fmt.Errorf("unknown rule target: %v", target)
	}

	// Create a new rule which will be used to overwrite our current rule.
	newRule := &looprpc.LiquidityRule{}

	if ctx.IsSet("mininbound") {
		newRule.MinimumInbound = uint32(ctx.Int("mininbound"))
		newRule.Type = looprpc.LiquidityRuleType_THRESHOLD
	}

	if ctx.IsSet("minoutbound") {
		newRule.MinimumOutbound = uint32(ctx.Int("minoutbound"))
		newRule.Type = looprpc.LiquidityRuleType_THRESHOLD
	}

	ruleSet := newRule.Type != looprpc.LiquidityRuleType_UNKNOWN

	// Check that our rule is set when we need it, and not set when we have
	// no target.
	switch cfg.Target {
	case looprpc.LiquidityTarget_NONE:
		if ruleSet {
			return errors.New("target none should not have a " +
				"rule set")
		}

	default:
		if !ruleSet {
			return fmt.Errorf("target: %v requires rule ", target)
		}
	}

	// Update our config to use this new rule.
	cfg.Rule = newRule
	_, err = client.SetLiquidityConfig(
		context.Background(),
		&looprpc.SetLiquidityConfigRequest{Config: cfg},
	)

	return err
}

var suggestSwapCommand = cli.Command{
	Name:  "suggestswaps",
	Usage: "show a list of suggested swaps",
	Description: "Displays a list of suggested swaps that aim to obtain " +
		"the liquidity thresholds set out in the autolooper config. ",
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
