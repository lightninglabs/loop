package loopd

import (
	"context"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightningnetwork/lnd/clock"
)

// getClient returns an instance of the swap client.
func getClient(config *Config, lnd *lndclient.LndServices) (*loop.Client,
	func(), error) {

	clientConfig := &loop.ClientConfig{
		ServerAddress:   config.Server.Host,
		ProxyAddress:    config.Server.Proxy,
		SwapServerNoTLS: config.Server.NoTLS,
		TLSPathServer:   config.Server.TLSPath,
		Lnd:             lnd,
		MaxLsatCost:     btcutil.Amount(config.MaxLSATCost),
		MaxLsatFee:      btcutil.Amount(config.MaxLSATFee),
		LoopOutMaxParts: config.LoopOutMaxParts,
	}

	swapClient, cleanUp, err := loop.NewClient(config.DataDir, clientConfig)
	if err != nil {
		return nil, nil, err
	}

	return swapClient, cleanUp, nil
}

func getLiquidityManager(client *loop.Client) *liquidity.Manager {
	mngrCfg := &liquidity.Config{
		LoopOutRestrictions: func(ctx context.Context) (
			*liquidity.Restrictions, error) {

			outTerms, err := client.Server.GetLoopOutTerms(ctx)
			if err != nil {
				return nil, err
			}

			return liquidity.NewRestrictions(
				outTerms.MinSwapAmount, outTerms.MaxSwapAmount,
			), nil
		},
		Lnd:                  client.LndServices,
		Clock:                clock.NewDefaultClock(),
		ListLoopOut:          client.Store.FetchLoopOutSwaps,
		ListLoopIn:           client.Store.FetchLoopInSwaps,
		MinimumConfirmations: minConfTarget,
	}

	return liquidity.NewManager(mngrCfg)
}
