package loopd

import (
	"os"
	"path/filepath"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/lndclient"
)

// getClient returns an instance of the swap client.
func getClient(config *config, lnd *lndclient.LndServices) (*loop.Client,
	func(), error) {

	storeDir, err := getStoreDir(config.Network)
	if err != nil {
		return nil, nil, err
	}

	swapClient, cleanUp, err := loop.NewClient(
		storeDir, config.SwapServer, config.Proxy, config.Insecure,
		config.TLSPathSwapSrv, lnd, btcutil.Amount(config.MaxLSATCost),
		btcutil.Amount(config.MaxLSATFee),
	)
	if err != nil {
		return nil, nil, err
	}

	return swapClient, cleanUp, nil
}

func getStoreDir(network string) (string, error) {
	dir := filepath.Join(loopDirBase, network)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return "", err
	}

	return dir, nil
}
