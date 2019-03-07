package main

type lndConfig struct {
	Host         string `long:"host" description:"lnd instance rpc address"`
	MacaroonPath string `long:"macaroonpath" description:"Path to lnd macaroon"`
	TLSPath      string `long:"tlspath" description:"Path to lnd tls certificate"`
}

type viewParameters struct{}

type config struct {
	Insecure   bool   `long:"insecure" description:"disable tls"`
	Network    string `long:"network" description:"network to run on" choice:"regtest" choice:"testnet" choice:"mainnet" choice:"simnet"`
	SwapServer string `long:"swapserver" description:"swap server address host:port"`
	Listen     string `long:"listen" description:"address to listen on for rpc lcients"`

	Lnd *lndConfig `group:"lnd" namespace:"lnd"`

	View viewParameters `command:"view" alias:"v" description:"View all swaps in the database. This command can only be executed when loopd is not running."`
}

var defaultConfig = config{
	Network:    "mainnet",
	SwapServer: "swap.lightning.today:11009",
	Listen:     "localhost:11010",
	Insecure:   false,
	Lnd: &lndConfig{
		Host: "localhost:10009",
	},
}
