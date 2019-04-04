package main

import (
	"fmt"
	"strconv"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/swap"
)

// view prints all swaps currently in the database.
func view(config *config) error {
	chainParams, err := swap.ChainParamsFromNetwork(config.Network)
	if err != nil {
		return err
	}

	lnd, err := getLnd(config.Network, config.Lnd)
	if err != nil {
		return err
	}
	defer lnd.Close()

	swapClient, cleanup, err := getClient(
		config.Network, config.SwapServer, config.Insecure, &lnd.LndServices,
	)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := viewOut(swapClient, chainParams); err != nil {
		return err
	}

	if err := viewIn(swapClient, chainParams); err != nil {
		return err
	}

	return nil
}

func viewOut(swapClient *loop.Client, chainParams *chaincfg.Params) error {
	swaps, err := swapClient.FetchLoopOutSwaps()
	if err != nil {
		return err
	}

	for _, s := range swaps {
		htlc, err := swap.NewHtlc(
			s.Contract.CltvExpiry,
			s.Contract.SenderKey,
			s.Contract.ReceiverKey,
			s.Hash, swap.HtlcP2WSH, chainParams,
		)
		if err != nil {
			return err
		}

		fmt.Printf("OUT %v\n", s.Hash)
		fmt.Printf("   Created: %v (height %v)\n",
			s.Contract.InitiationTime, s.Contract.InitiationHeight,
		)
		fmt.Printf("   Preimage: %v\n", s.Contract.Preimage)
		fmt.Printf("   Htlc address: %v\n", htlc.Address)

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

func viewIn(swapClient *loop.Client, chainParams *chaincfg.Params) error {
	swaps, err := swapClient.FetchLoopInSwaps()
	if err != nil {
		return err
	}

	for _, s := range swaps {
		htlc, err := swap.NewHtlc(
			s.Contract.CltvExpiry,
			s.Contract.SenderKey,
			s.Contract.ReceiverKey,
			s.Hash, swap.HtlcNP2WSH, chainParams,
		)
		if err != nil {
			return err
		}

		fmt.Printf("IN %v\n", s.Hash)
		fmt.Printf("   Created: %v (height %v)\n",
			s.Contract.InitiationTime, s.Contract.InitiationHeight,
		)
		fmt.Printf("   Preimage: %v\n", s.Contract.Preimage)
		fmt.Printf("   Htlc address: %v\n", htlc.Address)
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
