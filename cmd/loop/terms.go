package main

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/swap"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var termsCommand = cli.Command{
	Name:   "terms",
	Usage:  "show current server swap terms",
	Action: terms,
}

func terms(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	terms, err := client.GetLoopOutTerms(
		context.Background(), &looprpc.TermsRequest{},
	)
	if err != nil {
		return err
	}

	fmt.Printf("Amount: %d - %d\n",
		btcutil.Amount(terms.MinSwapAmount),
		btcutil.Amount(terms.MaxSwapAmount),
	)
	if err != nil {
		return err
	}

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
	printTerms(terms)

	return nil
}
