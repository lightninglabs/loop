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
	RPCListen  string `long:"rpclisten" description:"Address to listen on for gRPC clients"`
	RESTListen string `long:"restlisten" description:"Address to listen on for REST clients"`

	Lnd *lndConfig `group:"lnd" namespace:"lnd"`

	View viewParameters `command:"view" alias:"v" description:"View all swaps in the database. This command can only be executed when loopd is not running."`
}

var defaultConfig = config{
	Network:    "mainnet",
	SwapServer: "swap.lightning.today:11009",
	RPCListen:  "localhost:11010",
	RESTListen: "localhost:8081",
	Insecure:   false,
	Lnd: &lndConfig{
		Host: "localhost:10009",
	},
}
