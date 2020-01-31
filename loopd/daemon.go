package loopd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/pprof"
	"sync"
	"time"

	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
)

// listenerCfg holds closures used to retrieve listeners for the gRPC services.
type listenerCfg struct {
	// grpcListener returns a listener to use for the gRPC server.
	grpcListener func() (net.Listener, error)

	// restListener returns a listener to use for the REST proxy.
	restListener func() (net.Listener, error)

	// getLnd returns a grpc connection to an lnd instance.
	getLnd func(string, *lndConfig) (*lndclient.GrpcLndServices, error)
}

// daemon runs loopd in daemon mode. It will listen for grpc connections,
// execute commands and pass back swap status information.
func daemon(config *config, lisCfg *listenerCfg) error {
	lnd, err := lisCfg.getLnd(config.Network, config.Lnd)
	if err != nil {
		return err
	}
	defer lnd.Close()

	// If no swap server is specified, use the default addresses for mainnet
	// and testnet.
	if config.SwapServer == "" {
		switch config.Network {
		case "mainnet":
			config.SwapServer = mainnetServer
		case "testnet":
			config.SwapServer = testnetServer
		default:
			return errors.New("no swap server address specified")
		}
	}

	log.Infof("Swap server address: %v", config.SwapServer)

	// Create an instance of the loop client library.
	swapClient, cleanup, err := getClient(config, &lnd.LndServices)
	if err != nil {
		return err
	}
	defer cleanup()

	// Retrieve all currently existing swaps from the database.
	swapsList, err := swapClient.FetchSwaps()
	if err != nil {
		return err
	}

	swaps := make(map[lntypes.Hash]loop.SwapInfo)
	for _, s := range swapsList {
		swaps[s.SwapHash] = *s
	}

	// Instantiate the loopd gRPC server.
	server := swapClientServer{
		impl:        swapClient,
		lnd:         &lnd.LndServices,
		swaps:       swaps,
		subscribers: make(map[int]chan<- interface{}),
		statusChan:  make(chan loop.SwapInfo),
	}

	serverOpts := []grpc.ServerOption{}
	grpcServer := grpc.NewServer(serverOpts...)
	looprpc.RegisterSwapClientServer(grpcServer, &server)

	// Next, start the gRPC server listening for HTTP/2 connections.
	log.Infof("Starting gRPC listener")
	grpcListener, err := lisCfg.grpcListener()
	if err != nil {
		return fmt.Errorf("RPC server unable to listen on %s",
			config.RPCListen)

	}
	defer grpcListener.Close()

	// The default JSON marshaler of the REST proxy only sets OrigName to
	// true, which instructs it to use the same field names as specified in
	// the proto file and not switch to camel case. What we also want is
	// that the marshaler prints all values, even if they are falsey.
	customMarshalerOption := proxy.WithMarshalerOption(
		proxy.MIMEWildcard, &proxy.JSONPb{
			OrigName:     true,
			EmitDefaults: true,
		},
	)

	// We'll also create and start an accompanying proxy to serve clients
	// through REST.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mux := proxy.NewServeMux(customMarshalerOption)
	proxyOpts := []grpc.DialOption{grpc.WithInsecure()}
	err = looprpc.RegisterSwapClientHandlerFromEndpoint(
		ctx, mux, config.RPCListen, proxyOpts,
	)
	if err != nil {
		return err
	}

	restListener, err := lisCfg.restListener()
	if err != nil {
		return fmt.Errorf("REST proxy unable to listen on %s",
			config.RESTListen)
	}

	// A nil listener indicates REST is disabled.
	if restListener != nil {
		log.Infof("Starting REST proxy listener")

		defer restListener.Close()
		proxy := &http.Server{Handler: mux}

		go func() {
			err := proxy.Serve(restListener)
			// ErrServerClosed is always returned when the proxy is
			// shut down, so don't log it.
			if err != nil && err != http.ErrServerClosed {
				log.Error(err)
			}
		}()
	} else {
		log.Infof("REST proxy disabled")
	}

	mainCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// Start the swap client itself.
	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Infof("Starting swap client")
		err := swapClient.Run(mainCtx, server.statusChan)
		if err != nil {
			log.Error(err)
		}
		log.Infof("Swap client stopped")

		log.Infof("Stopping gRPC server")
		grpcServer.Stop()

		cancel()
	}()

	// Start a goroutine that broadcasts swap updates to clients.
	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Infof("Waiting for updates")
		server.processStatusUpdates(mainCtx)
	}()

	// Start the grpc server.
	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Infof("RPC server listening on %s", grpcListener.Addr())

		if restListener != nil {
			log.Infof("REST proxy listening on %s", restListener.Addr())
		}

		err = grpcServer.Serve(grpcListener)
		if err != nil {
			log.Error(err)
		}
	}()

	interruptChannel := make(chan os.Signal, 1)
	signal.Notify(interruptChannel, os.Interrupt)

	// Run until the users terminates loopd or an error occurred.
	select {
	case <-interruptChannel:
		log.Infof("Received SIGINT (Ctrl+C).")

		// TODO: Remove debug code.
		// Debug code to dump goroutines on hanging exit.
		go func() {
			time.Sleep(5 * time.Second)
			pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
		}()

		cancel()
	case <-mainCtx.Done():
	}

	wg.Wait()

	return nil
}
