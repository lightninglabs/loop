package loopd

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/jessevdk/go-flags"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/signal"
)

const defaultConfigFilename = "loopd.conf"

var (
	// LoopMinRequiredLndVersion is the minimum required version of lnd that
	// is compatible with the current version of the loop client. Also all
	// listed build tags/subservers need to be enabled.
	LoopMinRequiredLndVersion = &verrpc.Version{
		AppMajor: 0,
		AppMinor: 10,
		AppPatch: 1,
		BuildTags: []string{
			"signrpc", "walletrpc", "chainrpc", "invoicesrpc",
		},
	}
)

// RPCConfig holds optional options that can be used to make the loop daemon
// communicate on custom connections.
type RPCConfig struct {
	// RPCListener is an optional listener that if set will override the
	// daemon's gRPC settings, and make the gRPC server listen on this
	// listener.
	// Note that setting this will also disable REST.
	RPCListener net.Listener

	// LndConn is an optional connection to an lnd instance. If set it will
	// override the TCP connection created from daemon's config.
	LndConn net.Conn
}

// newListenerCfg creates and returns a new listenerCfg from the passed config
// and RPCConfig.
func newListenerCfg(config *Config, rpcCfg RPCConfig) *listenerCfg {
	return &listenerCfg{
		grpcListener: func() (net.Listener, error) {
			// If a custom RPC listener is set, we will listen on
			// it instead of the regular tcp socket.
			if rpcCfg.RPCListener != nil {
				return rpcCfg.RPCListener, nil
			}

			return net.Listen("tcp", config.RPCListen)
		},
		restListener: func() (net.Listener, error) {
			// If a custom RPC listener is set, we disable REST.
			if rpcCfg.RPCListener != nil {
				return nil, nil
			}

			return net.Listen("tcp", config.RESTListen)
		},
		getLnd: func(network lndclient.Network, cfg *lndConfig) (
			*lndclient.GrpcLndServices, error) {

			svcCfg := &lndclient.LndServicesConfig{
				LndAddress:   cfg.Host,
				Network:      network,
				MacaroonDir:  cfg.MacaroonDir,
				TLSPath:      cfg.TLSPath,
				CheckVersion: LoopMinRequiredLndVersion,
			}

			// If a custom lnd connection is specified we use that
			// directly.
			if rpcCfg.LndConn != nil {
				svcCfg.Dialer = func(context.Context, string) (
					net.Conn, error) {
					return rpcCfg.LndConn, nil
				}
			}

			return lndclient.NewLndServices(svcCfg)
		},
	}
}

// Run starts the loop daemon and blocks until it's shut down again.
func Run(rpcCfg RPCConfig) error {
	config := DefaultConfig()

	// Parse command line flags.
	parser := flags.NewParser(&config, flags.Default)
	parser.SubcommandsOptional = true

	_, err := parser.Parse()
	if e, ok := err.(*flags.Error); ok && e.Type == flags.ErrHelp {
		return nil
	}
	if err != nil {
		return err
	}

	// Parse ini file.
	loopDir := lncfg.CleanAndExpandPath(config.LoopDir)
	configFile := lncfg.CleanAndExpandPath(config.ConfigFile)

	// If our loop directory is set and the config file parameter is not
	// set, we assume that they want to point to a config file in their
	// loop dir. However, if the config file has a non-default value, then
	// we leave the config parameter as its custom value.
	if loopDir != loopDirBase && configFile == defaultConfigFile {
		configFile = filepath.Join(
			loopDir, defaultConfigFilename,
		)
	}

	if err := flags.IniParse(configFile, &config); err != nil {
		// If it's a parsing related error, then we'll return
		// immediately, otherwise we can proceed as possibly the config
		// file doesn't exist which is OK.
		if _, ok := err.(*flags.IniError); ok {
			return err
		}
	}

	// Parse command line flags again to restore flags overwritten by ini
	// parse.
	_, err = parser.Parse()
	if err != nil {
		return err
	}

	// Show the version and exit if the version flag was specified.
	appName := filepath.Base(os.Args[0])
	appName = strings.TrimSuffix(appName, filepath.Ext(appName))
	if config.ShowVersion {
		fmt.Println(appName, "version", loop.Version())
		os.Exit(0)
	}

	// Special show command to list supported subsystems and exit.
	if config.DebugLevel == "show" {
		fmt.Printf("Supported subsystems: %v\n",
			logWriter.SupportedSubsystems())
		os.Exit(0)
	}

	// Validate our config before we proceed.
	if err := Validate(&config); err != nil {
		return err
	}

	// Initialize logging at the default logging level.
	err = logWriter.InitLogRotator(
		filepath.Join(config.LogDir, defaultLogFilename),
		config.MaxLogFileSize, config.MaxLogFiles,
	)
	if err != nil {
		return err
	}
	err = build.ParseAndSetDebugLevels(config.DebugLevel, logWriter)
	if err != nil {
		return err
	}

	// Print the version before executing either primary directive.
	log.Infof("Version: %v", loop.Version())

	lisCfg := newListenerCfg(&config, rpcCfg)

	// Execute command.
	if parser.Active == nil {
		signal.Intercept()

		daemon := New(&config, lisCfg)
		if err := daemon.Start(); err != nil {
			return err
		}

		select {
		case <-signal.ShutdownChannel():
			log.Infof("Received SIGINT (Ctrl+C).")
			daemon.Stop()

			// The above stop will return immediately. But we'll be
			// notified on the error channel once the process is
			// complete.
			return <-daemon.ErrChan

		case err := <-daemon.ErrChan:
			return err
		}
	}

	if parser.Active.Name == "view" {
		return view(&config, lisCfg)
	}

	return fmt.Errorf("unimplemented command %v", parser.Active.Name)
}
