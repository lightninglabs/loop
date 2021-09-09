package loopd

import (
	"context"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/ticker"
)

// getClient returns an instance of the swap client.
func getClient(config *Config, lnd *lndclient.LndServices) (*loop.Client,
	func(), error) {

	clientConfig := &loop.ClientConfig{
		ServerAddress:       config.Server.Host,
		ProxyAddress:        config.Server.Proxy,
		SwapServerNoTLS:     config.Server.NoTLS,
		TLSPathServer:       config.Server.TLSPath,
		Lnd:                 lnd,
		MaxLsatCost:         btcutil.Amount(config.MaxLSATCost),
		MaxLsatFee:          btcutil.Amount(config.MaxLSATFee),
		LoopOutMaxParts:     config.LoopOutMaxParts,
		LoopOutRoutingHints: config.LoopOutRoutingHints,
	}

	swapClient, cleanUp, err := loop.NewClient(config.DataDir, clientConfig)
	if err != nil {
		return nil, nil, err
	}

	return swapClient, cleanUp, nil
}

func getLiquidityManager(client *loop.Client) *liquidity.Manager {
	mngrCfg := &liquidity.Config{
		AutoloopTicker: ticker.NewForce(liquidity.DefaultAutoloopTicker),
		LoopOut:        client.LoopOut,
		Restrictions: func(ctx context.Context,
			swapType swap.Type) (*liquidity.Restrictions, error) {

			if swapType == swap.TypeOut {
				outTerms, err := client.Server.GetLoopOutTerms(ctx)
				if err != nil {
					return nil, err
				}

				return liquidity.NewRestrictions(
					outTerms.MinSwapAmount, outTerms.MaxSwapAmount,
				), nil
			}

			inTerms, err := client.Server.GetLoopInTerms(ctx)
			if err != nil {
				return nil, err
			}

			return liquidity.NewRestrictions(
				inTerms.MinSwapAmount, inTerms.MaxSwapAmount,
			), nil
		},
		Lnd:                  client.LndServices,
		Clock:                clock.NewDefaultClock(),
		LoopOutQuote:         client.LoopOutQuote,
		ListLoopOut:          client.Store.FetchLoopOutSwaps,
		ListLoopIn:           client.Store.FetchLoopInSwaps,
		MinimumConfirmations: minConfTarget,
	}

	return liquidity.NewManager(mngrCfg)
}
