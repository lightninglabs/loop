package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/urfave/cli"
)

var listSwapsCommand = cli.Command{
	Name:  "listswaps",
	Usage: "list all swaps in the local database",
	Description: "Allows the user to get a list of all swaps that are " +
		"currently stored in the database",
	Action: listSwaps,
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "loop_out_only",
			Usage: "only list swaps that are loop out swaps",
		},
		cli.BoolFlag{
			Name:  "loop_in_only",
			Usage: "only list swaps that are loop in swaps",
		},
		cli.BoolFlag{
			Name:  "pending_only",
			Usage: "only list pending swaps",
		},
		labelFlag,
		channelFlag,
		lastHopFlag,
		cli.Uint64Flag{
			Name:  "max_swaps",
			Usage: "Max number of swaps to return after filtering",
		},
		cli.Int64Flag{
			Name: "start_time_ns",
			Usage: "Unix timestamp in nanoseconds to select swaps initiated " +
				"after this time",
		},
	},
}

func listSwaps(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	if ctx.Bool("loop_out_only") && ctx.Bool("loop_in_only") {
		return fmt.Errorf("only one of loop_out_only and loop_in_only " +
			"can be set")
	}

	filter := &looprpc.ListSwapsFilter{}

	// Set the swap type filter.
	switch {
	case ctx.Bool("loop_out_only"):
		filter.SwapType = looprpc.ListSwapsFilter_LOOP_OUT
	case ctx.Bool("loop_in_only"):
		filter.SwapType = looprpc.ListSwapsFilter_LOOP_IN
	}

	// Set the pending only filter.
	filter.PendingOnly = ctx.Bool("pending_only")

	// Parse outgoing channel set. Don't string split if the flag is empty.
	// Otherwise, strings.Split returns a slice of length one with an empty
	// element.
	var outgoingChanSet []uint64
	if ctx.IsSet(channelFlag.Name) {
		chanStrings := strings.Split(ctx.String(channelFlag.Name), ",")
		for _, chanString := range chanStrings {
			chanID, err := strconv.ParseUint(chanString, 10, 64)
			if err != nil {
				return fmt.Errorf("error parsing channel id "+
					"\"%v\"", chanString)
			}
			outgoingChanSet = append(outgoingChanSet, chanID)
		}
		filter.OutgoingChanSet = outgoingChanSet
	}

	// Parse last hop.
	var lastHop []byte
	if ctx.IsSet(lastHopFlag.Name) {
		lastHopVertex, err := route.NewVertexFromStr(
			ctx.String(lastHopFlag.Name),
		)
		if err != nil {
			return err
		}

		lastHop = lastHopVertex[:]
		filter.LoopInLastHop = lastHop
	}

	// Parse label.
	if ctx.IsSet(labelFlag.Name) {
		filter.Label = ctx.String(labelFlag.Name)
	}

	// Parse start timestamp if set.
	if ctx.IsSet("start_time_ns") {
		filter.StartTimestampNs = ctx.Int64("start_time_ns")
	}

	resp, err := client.ListSwaps(
		context.Background(), &looprpc.ListSwapsRequest{
			ListSwapFilter: filter,
			MaxSwaps:       ctx.Uint64("max_swaps"),
		},
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
