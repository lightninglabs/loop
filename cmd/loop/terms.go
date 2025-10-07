package main

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

var termsCommand = &cli.Command{
	Name:   "terms",
	Usage:  "Display the current swap terms imposed by the server.",
	Action: terms,
}

func terms(ctx context.Context, cmd *cli.Command) error {
	client, cleanup, err := getClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	printAmountRange := func(minAmt, maxAmt int64) {
		fmt.Printf("Amount: %d - %d\n",
			btcutil.Amount(minAmt), btcutil.Amount(maxAmt),
		)
	}

	fmt.Println("Loop Out")
	fmt.Println("--------")
	req := &looprpc.TermsRequest{}
	loopOutTerms, err := client.LoopOutTerms(ctx, req)
	if err != nil {
		fmt.Println(err)
	} else {
		printAmountRange(
			loopOutTerms.MinSwapAmount,
			loopOutTerms.MaxSwapAmount,
		)
		fmt.Printf("Cltv delta: %d - %d\n",
			loopOutTerms.MinCltvDelta, loopOutTerms.MaxCltvDelta,
		)
	}

	fmt.Println()

	fmt.Println("Loop In")
	fmt.Println("------")
	loopInTerms, err := client.GetLoopInTerms(
		ctx, &looprpc.TermsRequest{},
	)
	if err != nil {
		fmt.Println(err)
	} else {
		printAmountRange(
			loopInTerms.MinSwapAmount,
			loopInTerms.MaxSwapAmount,
		)
	}

	return nil
}
