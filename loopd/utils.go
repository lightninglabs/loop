package loopd

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/ticker"
)

// getClient returns an instance of the swap client.
func getClient(cfg *Config, swapDb loopdb.SwapStore,
	lnd *lndclient.LndServices) (*loop.Client, func(), error) {

	clientConfig := &loop.ClientConfig{
		ServerAddress:       cfg.Server.Host,
		ProxyAddress:        cfg.Server.Proxy,
		SwapServerNoTLS:     cfg.Server.NoTLS,
		TLSPathServer:       cfg.Server.TLSPath,
		Lnd:                 lnd,
		MaxLsatCost:         btcutil.Amount(cfg.MaxLSATCost),
		MaxLsatFee:          btcutil.Amount(cfg.MaxLSATFee),
		LoopOutMaxParts:     cfg.LoopOutMaxParts,
		TotalPaymentTimeout: cfg.TotalPaymentTimeout,
		MaxPaymentRetries:   cfg.MaxPaymentRetries,
	}

	swapClient, cleanUp, err := loop.NewClient(
		cfg.DataDir, swapDb, clientConfig,
	)
	if err != nil {
		return nil, nil, err
	}

	return swapClient, cleanUp, nil
}

func openDatabase(cfg *Config, chainParams *chaincfg.Params) (loopdb.SwapStore,
	*loopdb.BaseDB, error) { //nolint:unparam

	var (
		db     loopdb.SwapStore
		err    error
		baseDb loopdb.BaseDB
	)
	switch cfg.DatabaseBackend {
	case DatabaseBackendSqlite:
		log.Infof("Opening sqlite3 database at: %v",
			cfg.Sqlite.DatabaseFileName)
		db, err = loopdb.NewSqliteStore(
			cfg.Sqlite, chainParams,
		)
		baseDb = *db.(*loopdb.SqliteSwapStore).BaseDB

	case DatabaseBackendPostgres:
		log.Infof("Opening postgres database at: %v",
			cfg.Postgres.DSN(true))
		db, err = loopdb.NewPostgresStore(
			cfg.Postgres, chainParams,
		)
		baseDb = *db.(*loopdb.PostgresStore).BaseDB

	default:
		return nil, nil, fmt.Errorf("unknown database backend: %s",
			cfg.DatabaseBackend)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("unable to open database: %v", err)
	}

	return db, &baseDb, nil
}

func getLiquidityManager(client *loop.Client) *liquidity.Manager {
	mngrCfg := &liquidity.Config{
		AutoloopTicker: ticker.NewForce(liquidity.DefaultAutoloopTicker),
		LoopOut:        client.LoopOut,
		LoopIn:         client.LoopIn,
		Restrictions: func(ctx context.Context, swapType swap.Type,
			initiator string) (*liquidity.Restrictions, error) {

			if swapType == swap.TypeOut {
				outTerms, err := client.Server.GetLoopOutTerms(ctx, initiator)
				if err != nil {
					return nil, err
				}

				return liquidity.NewRestrictions(
					outTerms.MinSwapAmount, outTerms.MaxSwapAmount,
				), nil
			}

			inTerms, err := client.Server.GetLoopInTerms(ctx, initiator)
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
		LoopInQuote:          client.LoopInQuote,
		ListLoopOut:          client.Store.FetchLoopOutSwaps,
		GetLoopOut:           client.Store.FetchLoopOutSwap,
		ListLoopIn:           client.Store.FetchLoopInSwaps,
		LoopInTerms:          client.LoopInTerms,
		LoopOutTerms:         client.LoopOutTerms,
		MinimumConfirmations: minConfTarget,
		PutLiquidityParams:   client.Store.PutLiquidityParams,
		FetchLiquidityParams: client.Store.FetchLiquidityParams,
	}

	return liquidity.NewManager(mngrCfg)
}
