package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

var staticAssetCommand = &cli.Command{
	Name: "asset", Usage: "Fund and swap native asset static deposits",
	Commands: []*cli.Command{
		{
			Name:  "refund",
			Usage: "Refund an expired unreserved asset deposit",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "utxo", Required: true},
			},
			Action: refundAssetDeposit,
		},
		{
			Name: "deposit", Usage: "Queue an asset deposit",
			Flags: assetRequestFlags(
				false,
			), Action: createAssetDeposit,
		},
		{
			Name:  "in",
			Usage: "Swap one whole asset deposit to Lightning",
			Flags: assetRequestFlags(
				true,
			), Action: assetStaticLoopIn,
		},
		{
			Name:   "deposits",
			Usage:  "List asset deposits and reservations",
			Action: listAssetDeposits,
		},
		{
			Name:   "swaps",
			Usage:  "List durable asset requests and swaps",
			Action: listAssetSwaps,
		},
	},
}

func assetRequestFlags(swap bool) []cli.Flag {
	amountUsage := "Native asset units to deposit"
	if swap {
		amountUsage = "Exact native Lightning payout; " +
			"deposit excess is fee"
	}
	flags := []cli.Flag{
		&cli.StringFlag{
			Name: "request_id",
			Usage: "32-byte lowercase " +
				"hex identity; reuse this value after an " +
				"ambiguous response",
		},
		&cli.StringFlag{Name: "group_key", Required: true,
			Usage: "Compressed asset group key in lowercase hex"},
		&cli.Uint64Flag{Name: "amount", Required: true,
			Usage: amountUsage},
	}
	if swap {
		flags = append(flags,
			&cli.StringFlag{Name: "utxo", Required: true,
				Usage: "Canonical txid:index of one whole " +
					"deposit"},
			&cli.Uint64Flag{Name: "max_asset_debit",
				Usage: "Maximum native debit; defaults to " +
					"amount"},
			&cli.Uint64Flag{Name: "max_asset_fee",
				Usage: "Maximum native swap fee"},
		)
	}
	return flags
}

func canonicalAssetHex(value string, length int) ([]byte, error) {
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != length ||
		hex.EncodeToString(raw) != value {

		return nil, fmt.Errorf("invalid canonical asset identifier")
	}
	return raw, nil
}

func assetCommandIdentity(cmd *cli.Command) ([]byte, []byte, error) {
	group, err := canonicalAssetHex(cmd.String("group_key"), 33)
	if err != nil {
		return nil, nil, err
	}
	if _, err := btcec.ParsePubKey(group); err != nil {
		return nil, nil, fmt.Errorf("invalid asset group key")
	}
	if cmd.Uint64("amount") == 0 {
		return nil, nil, fmt.Errorf("asset amount must be positive")
	}
	id := make([]byte, 32)
	if value := cmd.String("request_id"); value != "" {
		id, err = canonicalAssetHex(value, 32)
		if err != nil {
			return nil, nil, err
		}
	} else if _, err := rand.Read(id); err != nil {
		return nil, nil, err
	}
	// Print before the RPC so an interrupted invocation remains retryable.
	fmt.Fprintf(os.Stderr, "Asset request ID: %x\n", id)
	return id, group, nil
}

func createAssetDeposit(ctx context.Context, cmd *cli.Command) error {
	id, group, err := assetCommandIdentity(cmd)
	if err != nil {
		return err
	}
	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	response, err := client.CreateAssetDeposit(ctx,
		&looprpc.CreateAssetDepositRequest{
			RequestId: id,
			GroupKey:  group,
			Amount:    cmd.Uint64("amount"),
		})
	if err != nil {
		return err
	}
	printRespJSON(response)
	return nil
}

// assetLoopInRPC sends no Bitcoin deposit selection. Older daemons reject
// the empty Bitcoin request when they ignore the unknown asset parameters.
func assetLoopInRPC(ctx context.Context, client looprpc.SwapClientClient,
	params *looprpc.AssetLoopInParams) (
	*looprpc.StaticAddressLoopInResponse, error) {

	response, err := client.StaticAddressLoopIn(ctx,
		&looprpc.StaticAddressLoopInRequest{AssetParams: params})
	if err != nil {
		return nil, err
	}
	if response.GetAssetRequest() == nil {
		return nil, fmt.Errorf("daemon did not accept the asset protocol")
	}
	return response, nil
}

func assetStaticLoopIn(ctx context.Context, cmd *cli.Command) error {
	id, group, err := assetCommandIdentity(cmd)
	if err != nil {
		return err
	}
	outpoint := cmd.String("utxo")
	point, err := wire.NewOutPointFromString(outpoint)
	if err != nil || point.String() != outpoint {
		return fmt.Errorf("invalid canonical deposit outpoint")
	}
	maxDebit := cmd.Uint64("max_asset_debit")
	if !cmd.IsSet("max_asset_debit") {
		maxDebit = cmd.Uint64("amount")
	}
	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	response, err := assetLoopInRPC(ctx, client,
		&looprpc.AssetLoopInParams{
			RequestId: id, GroupKey: group, Outpoint: outpoint,
			Amount: cmd.Uint64("amount"), MaxAssetDebit: maxDebit,
			MaxAssetFee: cmd.Uint64("max_asset_fee"),
		})
	if err != nil {
		return err
	}
	printRespJSON(response)
	return nil
}

func listAssetDeposits(ctx context.Context, cmd *cli.Command) error {
	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	response, err := client.ListStaticAddressDeposits(
		ctx, &looprpc.ListStaticAddressDepositsRequest{AssetsOnly: true},
	)
	if err != nil {
		return err
	}
	if !response.GetAssetsOnly() {
		return fmt.Errorf("daemon did not accept the asset protocol")
	}
	printRespJSON(response)
	return nil
}

func listAssetSwaps(ctx context.Context, cmd *cli.Command) error {
	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	response, err := client.ListStaticAddressSwaps(
		ctx, &looprpc.ListStaticAddressSwapsRequest{AssetsOnly: true},
	)
	if err != nil {
		return err
	}
	if !response.GetAssetsOnly() {
		return fmt.Errorf("daemon did not accept the asset protocol")
	}
	printRespJSON(response)
	return nil
}

func refundAssetDeposit(ctx context.Context, cmd *cli.Command) error {
	outpoint := cmd.String("utxo")
	point, err := wire.NewOutPointFromString(outpoint)
	if err != nil || point.String() != outpoint {
		return fmt.Errorf("invalid canonical deposit outpoint")
	}
	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	response, err := client.RefundAssetDeposit(
		ctx, &looprpc.RefundAssetDepositRequest{Outpoint: outpoint},
	)
	if err != nil {
		return err
	}
	printRespJSON(response)
	return nil
}
