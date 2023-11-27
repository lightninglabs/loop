package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/urfave/cli"
)

var listSwapsCommand = cli.Command{
	Name:  "listswaps",
	Usage: "list all swaps in the local database",
	Description: "Allows the user to get a list of all swaps that are " +
		"currently stored in the database",
	Action: listSwaps,
}

func listSwaps(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListSwaps(
		context.Background(), &looprpc.ListSwapsRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var swapInfoCommand = cli.Command{
	Name:      "swapinfo",
	Usage:     "show the status of a swap",
	ArgsUsage: "id",
	Description: "Allows the user to get the status of a single swap " +
		"currently stored in the database",
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name:  "id",
			Usage: "the ID of the swap",
		},
	},
	Action: swapInfo,
}

func swapInfo(ctx *cli.Context) error {
	args := ctx.Args()

	var id string
	switch {
	case ctx.IsSet("id"):
		id = ctx.String("id")
	case ctx.NArg() > 0:
		id = args[0]
		args = args.Tail() // nolint:wastedassign
	default:
		// Show command help if no arguments and flags were provided.
		return cli.ShowCommandHelp(ctx, "swapinfo")
	}

	if len(id) != hex.EncodedLen(lntypes.HashSize) {
		return fmt.Errorf("invalid swap ID")
	}
	idBytes, err := hex.DecodeString(id)
	if err != nil {
		return fmt.Errorf("cannot hex decode id: %v", err)
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.SwapInfo(
		context.Background(), &looprpc.SwapInfoRequest{Id: idBytes},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var abandonSwapCommand = cli.Command{
	Name:  "abandonswap",
	Usage: "abandon a swap with a given swap hash",
	Description: "This command overrides the database and abandons a " +
		"swap with a given swap hash.\n\n" +
		"!!! This command might potentially lead to loss of funds if " +
		"it is applied to swaps that are still waiting for pending " +
		"user funds. Before executing this command make sure that " +
		"no funds are locked by the swap.",
	ArgsUsage: "ID",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "i_know_what_i_am_doing",
			Usage: "Specify this flag if you made sure that you " +
				"read and understood the following " +
				"consequence of applying this command.",
		},
	},
	Action: abandonSwap,
}

func abandonSwap(ctx *cli.Context) error {
	args := ctx.Args()

	var id string
	switch {
	case ctx.IsSet("id"):
		id = ctx.String("id")

	case ctx.NArg() > 0:
		id = args[0]
		args = args.Tail() // nolint:wastedassign

	default:
		// Show command help if no arguments and flags were provided.
		return cli.ShowCommandHelp(ctx, "abandonswap")
	}

	if len(id) != hex.EncodedLen(lntypes.HashSize) {
		return fmt.Errorf("invalid swap ID")
	}
	idBytes, err := hex.DecodeString(id)
	if err != nil {
		return fmt.Errorf("cannot hex decode id: %v", err)
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	if !ctx.Bool("i_know_what_i_am_doing") {
		return cli.ShowCommandHelp(ctx, "abandonswap")
	}

	resp, err := client.AbandonSwap(
		context.Background(), &looprpc.AbandonSwapRequest{
			Id:                idBytes,
			IKnowWhatIAmDoing: ctx.Bool("i_know_what_i_am_doing"),
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
