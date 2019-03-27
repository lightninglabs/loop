package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
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

	printTerms := func(terms *looprpc.TermsResponse) {
		fmt.Printf("Amount: %d - %d\n",
			btcutil.Amount(terms.MinSwapAmount),
			btcutil.Amount(terms.MaxSwapAmount),
		)
		fmt.Printf("Fee:    %d + %.4f %% (%d prepaid)\n",
			btcutil.Amount(terms.SwapFeeBase),
			swap.FeeRateAsPercentage(terms.SwapFeeRate),
			btcutil.Amount(terms.PrepayAmt),
		)

		fmt.Printf("Cltv delta:   %v blocks\n", terms.CltvDelta)
	}

	fmt.Println("Loop Out")
	fmt.Println("--------")
	req := &looprpc.TermsRequest{}
	loopOutTerms, err := client.LoopOutTerms(context.Background(), req)
	if err != nil {
		fmt.Println(err)
	} else {
		printTerms(loopOutTerms)
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
		printTerms(loopInTerms)
	}

	return nil
}
