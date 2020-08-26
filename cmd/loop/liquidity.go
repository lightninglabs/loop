package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/lightningnetwork/lnd/routing/route"

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
		cli.BoolFlag{
			Name:  "exclude",
			Usage: "exclude the target from swap suggestions",
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

	// Create a function which will be used to set a custom rule.
	// TODO(carla): this whole thing can be expressed more cleanly
	var setCfgRule func(rule *looprpc.LiquidityRule)

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
		peer, err := route.NewVertexFromStr(target)
		isPeer := err == nil

		chanID, err := strconv.Atoi(target)
		isChannel := err == nil

		switch {
		case isPeer:
			setCfgRule = func(rule *looprpc.LiquidityRule) {
				if cfg.PeerRules == nil {
					cfg.PeerRules = make(
						map[string]*looprpc.LiquidityRule,
					)
				}

				cfg.PeerRules[peer.String()] = rule
			}

		case isChannel:
			setCfgRule = func(rule *looprpc.LiquidityRule) {
				if cfg.ChannelRules == nil {
					cfg.ChannelRules = make(
						map[uint64]*looprpc.LiquidityRule,
					)
				}

				cfg.ChannelRules[uint64(chanID)] = rule
			}

		default:
			return fmt.Errorf("unknown custom target: %v, "+
				"please set peer-pubkey or channel", target)
		}
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

	// Check if our exclude flag is set. If the rule type has already been
	// set at this stage, we know that fields belonging to another rule
	// have been set, so we fail.
	if ctx.IsSet("exclude") {
		if newRule.Type != looprpc.LiquidityRuleType_UNKNOWN {
			return fmt.Errorf("do not set any fields with exclude")
		}

		newRule.Type = looprpc.LiquidityRuleType_EXCLUDE
	}

	ruleSet := newRule.Type != looprpc.LiquidityRuleType_UNKNOWN

	// Check that our rule is set when we need it, and not set when we have
	// no target and do not have a custom rule.
	switch cfg.Target {
	case looprpc.LiquidityTarget_NONE:
		if ruleSet && setCfgRule == nil {
			return errors.New("target none should not have a " +
				"rule set")
		}

	default:
		if !ruleSet {
			return fmt.Errorf("target: %v requires rule ", target)
		}
	}

	// Update our config to use this new rule or add a custom rule.
	if setCfgRule == nil {
		cfg.Rule = newRule
	} else {
		setCfgRule(newRule)
	}

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
