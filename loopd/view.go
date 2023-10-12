package loopd

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopdb"
)

// view prints all swaps currently in the database.
func view(config *Config, lisCfg *ListenerCfg) error {
	network := lndclient.Network(config.Network)

	lnd, err := lisCfg.getLnd(network, config.Lnd)
	if err != nil {
		return err
	}
	defer lnd.Close()

	chainParams, err := network.ChainParams()
	if err != nil {
		return err
	}

	swapDb, _, err := openDatabase(config, chainParams)
	if err != nil {
		return err
	}

	swapClient, cleanup, err := getClient(config, swapDb, &lnd.LndServices)
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
	swaps, err := swapClient.Store.FetchLoopOutSwaps(context.Background())
	if err != nil {
		return err
	}

	for _, s := range swaps {
		s := s

		htlc, err := loop.GetHtlc(
			s.Hash, &s.Contract.SwapContract, chainParams,
		)
		if err != nil {
			return err
		}

		fmt.Printf("OUT %v\n", s.Hash)
		fmt.Printf("   Created: %v (height %v)\n",
			s.Contract.InitiationTime, s.Contract.InitiationHeight,
		)
		fmt.Printf("   Preimage: %v\n", s.Contract.Preimage)
		fmt.Printf("   Htlc address (%s): %v\n", htlc.OutputType,
			htlc.Address)

		fmt.Printf("   Uncharge channels: %v\n",
			s.Contract.OutgoingChanSet)
		fmt.Printf("   Dest: %v\n", s.Contract.DestAddr)
		fmt.Printf("   Amt: %v, Expiry: %v\n",
			s.Contract.AmountRequested, s.Contract.CltvExpiry,
		)
		for i, e := range s.Events {
			fmt.Printf("   Update %v, Time %v, State: %v",
				i, e.Time, e.State,
			)
			if e.State.Type() != loopdb.StateTypePending {
				fmt.Printf(", Cost: server=%v, onchain=%v, "+
					"offchain=%v",
					e.Cost.Server,
					e.Cost.Onchain,
					e.Cost.Offchain,
				)
			}

			fmt.Println()
		}
		fmt.Println()
	}

	return nil
}

func viewIn(swapClient *loop.Client, chainParams *chaincfg.Params) error {
	swaps, err := swapClient.Store.FetchLoopInSwaps(context.Background())
	if err != nil {
		return err
	}

	for _, s := range swaps {
		s := s

		htlc, err := loop.GetHtlc(
			s.Hash, &s.Contract.SwapContract, chainParams,
		)
		if err != nil {
			return err
		}

		fmt.Printf("IN %v\n", s.Hash)
		fmt.Printf("   Created: %v (height %v)\n",
			s.Contract.InitiationTime, s.Contract.InitiationHeight,
		)
		fmt.Printf("   Preimage: %v\n", s.Contract.Preimage)
		fmt.Printf("   Htlc address (%s): %v\n", htlc.OutputType,
			htlc.Address)
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
