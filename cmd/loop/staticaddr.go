package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var staticAddressCommands = cli.Command{
	Name:      "static",
	ShortName: "s",
	Usage:     "manage static loop-in addresses",
	Category:  "StaticAddress",
	Subcommands: []cli.Command{
		newStaticAddressCommand,
		listUnspentCommand,
		withdrawalCommand,
		summaryCommand,
	},
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

	resp, err := client.WithdrawDeposits(ctxb, &looprpc.WithdrawDepositsRequest{
		Outpoints: outpoints,
		All:       isAllSelected,
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
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
				"the specified state. The state can be one " +
				"of [deposited|withdrawing|withdrawn|" +
				"publish_expired_deposit|" +
				"wait_for_expiry_sweep|expired|failed].",
		},
	},
	Action: summary,
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

	case "publish_expired_deposit":
		filterState = looprpc.DepositState_PUBLISH_EXPIRED

	case "wait_for_expiry_sweep":
		filterState = looprpc.DepositState_WAIT_FOR_EXPIRY_SWEEP

	case "expired":
		filterState = looprpc.DepositState_EXPIRED

	case "failed":
		filterState = looprpc.DepositState_FAILED_STATE

	default:
		filterState = looprpc.DepositState_UNKNOWN_STATE
	}

	resp, err := client.GetStaticAddressSummary(
		ctxb, &looprpc.StaticAddressSummaryRequest{
			StateFilter: filterState,
		},
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
