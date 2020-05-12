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

	clientConfig := &loop.ClientConfig{
		ServerAddress:   config.SwapServer,
		ProxyAddress:    config.Proxy,
		Insecure:        config.Insecure,
		TLSPathServer:   config.TLSPathSwapSrv,
		Lnd:             lnd,
		MaxLsatCost:     btcutil.Amount(config.MaxLSATCost),
		MaxLsatFee:      btcutil.Amount(config.MaxLSATFee),
		LoopOutMaxParts: config.LoopOutMaxParts,
	}

	swapClient, cleanUp, err := loop.NewClient(storeDir, clientConfig)
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
