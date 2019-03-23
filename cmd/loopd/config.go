package main

import (
	"path/filepath"

	"github.com/btcsuite/btcutil"
)

var (
	loopDirBase = btcutil.AppDataDir("loop", false)

	defaultConfigFilename = "loopd.conf"

	defaultLogLevel    = "info"
	defaultLogDirname  = "logs"
	defaultLogFilename = "loopd.log"
	defaultLogDir      = filepath.Join(loopDirBase, defaultLogDirname)

	defaultMaxLogFiles    = 3
	defaultMaxLogFileSize = 10
)

type lndConfig struct {
	Host         string `long:"host" description:"lnd instance rpc address"`
	MacaroonPath string `long:"macaroonpath" description:"Path to lnd macaroon"`
	TLSPath      string `long:"tlspath" description:"Path to lnd tls certificate"`
}

type viewParameters struct{}

type config struct {
	ShowVersion bool   `short:"V" long:"version" description:"Display version information and exit"`
	Insecure    bool   `long:"insecure" description:"disable tls"`
	Network     string `long:"network" description:"network to run on" choice:"regtest" choice:"testnet" choice:"mainnet" choice:"simnet"`
	SwapServer  string `long:"swapserver" description:"swap server address host:port"`
	RPCListen   string `long:"rpclisten" description:"Address to listen on for gRPC clients"`
	RESTListen  string `long:"restlisten" description:"Address to listen on for REST clients"`

	Lnd *lndConfig `group:"lnd" namespace:"lnd"`

	View viewParameters `command:"view" alias:"v" description:"View all swaps in the database. This command can only be executed when loopd is not running."`

	LogDir         string `long:"logdir" description:"Directory to log output."`
	MaxLogFiles    int    `long:"maxlogfiles" description:"Maximum logfiles to keep (0 for no rotation)"`
	MaxLogFileSize int    `long:"maxlogfilesize" description:"Maximum logfile size in MB"`

	DebugLevel string `short:"d" long:"debuglevel" description:"Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems"`
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
	MaxLogFiles:    defaultMaxLogFiles,
	MaxLogFileSize: defaultMaxLogFileSize,
	DebugLevel:     defaultLogLevel,
	LogDir:         defaultLogDir,
}
