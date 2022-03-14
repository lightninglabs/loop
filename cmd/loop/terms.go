package main

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var termsCommand = cli.Command{
	Name:   "terms",
	Usage:  "Display the current swap terms imposed by the server.",
	Action: terms,
}

func terms(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	printAmountRange := func(min, max int64) {
		fmt.Printf("Amount: %d - %d\n",
			btcutil.Amount(min), btcutil.Amount(max),
		)
	}

	fmt.Println("Loop Out")
	fmt.Println("--------")
	req := &looprpc.TermsRequest{}
	loopOutTerms, err := client.LoopOutTerms(context.Background(), req)
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
		context.Background(), &looprpc.TermsRequest{},
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
