package main

import (
	"fmt"
	"strconv"

	"github.com/lightninglabs/nautilus/utils"
	"github.com/urfave/cli"
)

var viewCommand = cli.Command{
	Name: "view",
	Usage: `view all swaps in the database. This command can only be 
	executed when swapd is not running.`,
	Description: `
		Show all pending and completed swaps.`,
	Action: view,
}

// view prints all swaps currently in the database.
func view(ctx *cli.Context) error {
	network := ctx.GlobalString("network")

	chainParams, err := utils.ChainParamsFromNetwork(network)
	if err != nil {
		return err
	}

	lnd, err := getLnd(ctx)
	if err != nil {
		return err
	}
	defer lnd.Close()

	swapClient, cleanup, err := getClient(ctx, &lnd.LndServices)
	if err != nil {
		return err
	}
	defer cleanup()

	swaps, err := swapClient.GetUnchargeSwaps()
	if err != nil {
		return err
	}

	for _, s := range swaps {
		htlc, err := utils.NewHtlc(
			s.Contract.CltvExpiry,
			s.Contract.SenderKey,
			s.Contract.ReceiverKey,
			s.Hash,
		)
		if err != nil {
			return err
		}

		htlcAddress, err := htlc.Address(chainParams)
		if err != nil {
			return err
		}

		fmt.Printf("%v\n", s.Hash)
		fmt.Printf("   Created: %v (height %v)\n",
			s.Contract.InitiationTime, s.Contract.InitiationHeight,
		)
		fmt.Printf("   Preimage: %v\n", s.Contract.Preimage)
		fmt.Printf("   Htlc address: %v\n", htlcAddress)

		unchargeChannel := "any"
		if s.Contract.UnchargeChannel != nil {
			unchargeChannel = strconv.FormatUint(
				*s.Contract.UnchargeChannel, 10,
			)
		}
		fmt.Printf("   Uncharge channel: %v\n", unchargeChannel)
		fmt.Printf("   Dest: %v\n", s.Contract.DestAddr)
		fmt.Printf("   Amt: %v, Expiry: %v\n",
			s.Contract.AmountRequested, s.Contract.CltvExpiry,
		)
		for i, e := range s.Events {
			fmt.Printf("   Update %v, Time %v, State: %v\n",
				i, e.Time, e.State,
			)
		}
		fmt.Println()
	}

	return nil
}
