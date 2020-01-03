package loopd

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jessevdk/go-flags"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	defaultConfTarget = int32(6)
)

var (
	defaultConfigFilename = "loopd.conf"

	swaps            = make(map[lntypes.Hash]loop.SwapInfo)
	subscribers      = make(map[int]chan<- interface{})
	nextSubscriberID int
	swapsLock        sync.Mutex
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
func newListenerCfg(config *config, rpcCfg RPCConfig) *listenerCfg {
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
		getLnd: func(network string, cfg *lndConfig) (
			*lndclient.GrpcLndServices, error) {

			// If a custom lnd connection is specified we use that
			// directly.
			if rpcCfg.LndConn != nil {
				dialer := func(context.Context, string) (
					net.Conn, error) {
					return rpcCfg.LndConn, nil
				}

				return lndclient.NewLndServicesWithDialer(
					dialer,
					rpcCfg.LndConn.RemoteAddr().String(),
					network, cfg.MacaroonDir, cfg.TLSPath,
				)
			}

			return lndclient.NewLndServices(
				cfg.Host, network, cfg.MacaroonDir, cfg.TLSPath,
			)
		},
	}
}

func Start(rpcCfg RPCConfig) error {
	config := defaultConfig

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
	loopDir := filepath.Join(loopDirBase, config.Network)
	if err := os.MkdirAll(loopDir, os.ModePerm); err != nil {
		return err
	}

	configFile := filepath.Join(loopDir, defaultConfigFilename)
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

	// Append the network type to the log directory so it is
	// "namespaced" per network in the same fashion as the data directory.
	config.LogDir = filepath.Join(config.LogDir, config.Network)

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
		return daemon(&config, lisCfg)
	}

	if parser.Active.Name == "view" {
		return view(&config, lisCfg)
	}

	return fmt.Errorf("unimplemented command %v", parser.Active.Name)
}
