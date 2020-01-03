package loopd

import (
	"os"
	"path/filepath"

	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/lndclient"
)

// getLnd returns an instance of the lnd services proxy.
func getLnd(network string, cfg *lndConfig) (*lndclient.GrpcLndServices, error) {
	return lndclient.NewLndServices(
		cfg.Host, network, cfg.MacaroonDir, cfg.TLSPath,
	)
}

// getClient returns an instance of the swap client.
func getClient(network, swapServer string, insecure bool, tlsPathServer string,
	lnd *lndclient.LndServices) (*loop.Client, func(), error) {

	storeDir, err := getStoreDir(network)
	if err != nil {
		return nil, nil, err
	}

	swapClient, cleanUp, err := loop.NewClient(
		storeDir, swapServer, insecure, tlsPathServer, lnd,
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
