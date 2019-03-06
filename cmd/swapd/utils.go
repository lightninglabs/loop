package main

import (
	"os"
	"path/filepath"

	"github.com/lightninglabs/nautilus/client"
	"github.com/lightninglabs/nautilus/lndclient"
	"github.com/urfave/cli"
)

// getLnd returns an instance of the lnd services proxy.
func getLnd(ctx *cli.Context) (*lndclient.GrpcLndServices, error) {
	network := ctx.GlobalString("network")

	return lndclient.NewLndServices(ctx.GlobalString("lnd"),
		"client", network, ctx.GlobalString("macaroonpath"),
		ctx.GlobalString("tlspath"),
	)
}

// getClient returns an instance of the swap client.
func getClient(ctx *cli.Context, lnd *lndclient.LndServices) (*client.Client, func(), error) {
	network := ctx.GlobalString("network")

	storeDir, err := getStoreDir(network)
	if err != nil {
		return nil, nil, err
	}

	swapClient, cleanUp, err := client.NewClient(
		storeDir, ctx.GlobalString("swapserver"),
		ctx.GlobalBool("insecure"), lnd,
	)
	if err != nil {
		return nil, nil, err
	}

	return swapClient, cleanUp, nil
}

func getStoreDir(network string) (string, error) {
	dir := filepath.Join(defaultSwapletDir, network)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return "", err
	}

	return dir, nil
}
