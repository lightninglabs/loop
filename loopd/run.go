package loopd

import (
	"context"
	"crypto/tls"
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
		grpcListener: func(tlsCfg *tls.Config) (net.Listener, error) {
			// If a custom RPC listener is set, we will listen on
			// it instead of the regular tcp socket.
			if rpcCfg.RPCListener != nil {
				return rpcCfg.RPCListener, nil
			}

			listener, err := net.Listen("tcp", config.RPCListen)
			if err != nil {
				return nil, err
			}

			return tls.NewListener(listener, tlsCfg), nil
		},
		restListener: func(tlsCfg *tls.Config) (net.Listener, error) {
			// If a custom RPC listener is set, we disable REST.
			if rpcCfg.RPCListener != nil {
				return nil, nil
			}

			listener, err := net.Listen("tcp", config.RESTListen)
			if err != nil {
				return nil, err
			}

			return tls.NewListener(listener, tlsCfg), nil
		},
		getLnd: func(network lndclient.Network, cfg *lndConfig) (
			*lndclient.GrpcLndServices, error) {

			syncCtx, cancel := context.WithCancel(
				context.Background(),
			)
			defer cancel()

			svcCfg := &lndclient.LndServicesConfig{
				LndAddress:            cfg.Host,
				Network:               network,
				MacaroonDir:           cfg.MacaroonDir,
				TLSPath:               cfg.TLSPath,
				CheckVersion:          LoopMinRequiredLndVersion,
				BlockUntilChainSynced: true,
				ChainSyncCtx:          syncCtx,
			}

			// If a custom lnd connection is specified we use that
			// directly.
			if rpcCfg.LndConn != nil {
				svcCfg.Dialer = func(context.Context, string) (
					net.Conn, error) {
					return rpcCfg.LndConn, nil
				}
			}

			// Before we try to get our client connection, setup
			// a goroutine which will cancel our lndclient if loopd
			// is terminated, or exit if our context is cancelled.
			go func() {
				select {
				// If the client decides to kill loop before
				// lnd is synced, we cancel our context, which
				// will unblock lndclient.
				case <-signal.ShutdownChannel():
					cancel()

				// If our sync context was cancelled, we know
				// that the function exited, which means that
				// our client synced.
				case <-syncCtx.Done():
				}
			}()

			// This will block until lnd is synced to chain.
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
	configFile := getConfigPath(config, loopDir)

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

	// Start listening for signal interrupts regardless of which command
	// we are running. When our command tries to get a lnd connection, it
	// blocks until lnd is synced. We listen for interrupts so that we can
	// shutdown the daemon while waiting for sync to complete.
	if err := signal.Intercept(); err != nil {
		return err
	}

	// Execute command.
	if parser.Active == nil {
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

// getConfigPath gets our config path based on the values that are set in our
// config.
func getConfigPath(cfg Config, loopDir string) string {
	// If the config file path provided by the user is set, then we just
	// use this value.
	if cfg.ConfigFile != defaultConfigFile {
		return lncfg.CleanAndExpandPath(cfg.ConfigFile)
	}

	// If the user has set a loop directory that is different to the default
	// we will use this loop directory as the location of our config file.
	// We do not namespace by network, because this is a custom loop dir.
	if loopDir != LoopDirBase {
		return filepath.Join(loopDir, defaultConfigFilename)
	}

	// Otherwise, we are using our default loop directory, and the user did
	// not set a config file path. We use our default loop dir, namespaced
	// by network.
	return filepath.Join(loopDir, cfg.Network, defaultConfigFilename)
}
