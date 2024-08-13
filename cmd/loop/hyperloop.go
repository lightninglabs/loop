package main

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var hyperloopCommand = cli.Command{
	Name:  "hyperloop",
	Usage: "perform a fee optimized off-chain to on-chain swap (hyperloop)",
	Description: `

	`,
	ArgsUsage: "amt",
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name: "amt",
			Usage: "the amount in satoshis to loop out. To check " +
				"for the minimum and maximum amounts to loop " +
				"out please consult \"loop terms\"",
		},
		cli.StringFlag{
			Name: "addr",
			Usage: "the optional address that the looped out funds " +
				"should be sent to, if let blank the funds " +
				"will go to lnd's wallet",
		},
	},

	Action: hyperloop,
}

func hyperloop(ctx *cli.Context) error {
	args := ctx.Args()

	var amtStr string
	switch {
	case ctx.IsSet("amt"):
		amtStr = ctx.String("amt")

	case ctx.NArg() > 0:
		amtStr = args[0]

	default:
		// Show command help if no arguments and flags were provided.
		return cli.ShowCommandHelp(ctx, "hyperloop")
	}

	amt, err := parseAmt(amtStr)
	if err != nil {
		return err
	}

	// First set up the swap client itself.
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	ctxb := context.Background()

	hyperloopRes, err := client.HyperLoopOut(
		ctxb,
		&looprpc.HyperLoopOutRequest{
			Amt:             uint64(amt),
			CustomSweepAddr: ctx.String("addr"),
		},
	)
	if err != nil {
		return err
	}

	printJSON(hyperloopRes)

	return nil
}
