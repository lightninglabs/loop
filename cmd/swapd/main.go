package main

import (
	"fmt"
	"os"
	"sync"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/nautilus/client"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/urfave/cli"
)

const (
	defaultListenPort = 11010
	defaultConfTarget = int32(2)
)

var (
	defaultListenAddr = fmt.Sprintf("localhost:%d", defaultListenPort)
	defaultSwapletDir = btcutil.AppDataDir("swaplet", false)

	swaps            = make(map[lntypes.Hash]client.SwapInfo)
	subscribers      = make(map[int]chan<- interface{})
	nextSubscriberID int
	swapsLock        sync.Mutex
)

func main() {
	app := cli.NewApp()

	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "network",
			Value: "mainnet",
			Usage: "network to run on (regtest, testnet, mainnet)",
		},
		cli.StringFlag{
			Name:  "lnd",
			Value: "localhost:10009",
			Usage: "lnd instance rpc address host:port",
		},
		cli.StringFlag{
			Name:  "swapserver",
			Value: "swap.lightning.today:11009",
			Usage: "swap server address host:port",
		},
		cli.StringFlag{
			Name:  "macaroonpath",
			Usage: "path to lnd macaroon",
		},
		cli.StringFlag{
			Name:  "tlspath",
			Usage: "path to lnd tls certificate",
		},
		cli.BoolFlag{
			Name:  "insecure",
			Usage: "disable tls",
		},
	}
	app.Version = "0.0.1"
	app.Usage = "swaps execution daemon"
	app.Commands = []cli.Command{viewCommand}
	app.Action = daemon

	err := app.Run(os.Args)
	if err != nil {
		fmt.Println(err)
	}
}
