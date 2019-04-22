package main

import (
	"context"
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
	"github.com/lightninglabs/loop/looprpc"
	"google.golang.org/grpc"
)

// daemon runs loopd in daemon mode. It will listen for grpc connections,
// execute commands and pass back swap status information.
func daemon(config *config) error {
	lnd, err := getLnd(config.Network, config.Lnd)
	if err != nil {
		return err
	}
	defer lnd.Close()

	// If the user is targeting the testnet network, then we'll point
	// towards the testnet swap server rather than the mainnet endpoint.
	if config.Network == "testnet" {
		config.SwapServer = testnetServer
	}

	// Create an instance of the loop client library.
	swapClient, cleanup, err := getClient(
		config.Network, config.SwapServer, config.Insecure,
		&lnd.LndServices,
	)
	if err != nil {
		return err
	}
	defer cleanup()

	// Retrieve all currently existing swaps from the database.
	swapsList, err := swapClient.FetchSwaps()
	if err != nil {
		return err
	}

	for _, s := range swapsList {
		swaps[s.SwapHash] = *s
	}

	// Instantiate the loopd gRPC server.
	server := swapClientServer{
		impl: swapClient,
		lnd:  &lnd.LndServices,
	}

	serverOpts := []grpc.ServerOption{}
	grpcServer := grpc.NewServer(serverOpts...)
	looprpc.RegisterSwapClientServer(grpcServer, &server)

	// Next, start the gRPC server listening for HTTP/2 connections.
	logger.Infof("Starting gRPC listener")
	grpcListener, err := net.Listen("tcp", config.RPCListen)
	if err != nil {
		return fmt.Errorf("RPC server unable to listen on %s",
			config.RPCListen)

	}
	defer grpcListener.Close()

	// We'll also create and start an accompanying proxy to serve clients
	// through REST.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mux := proxy.NewServeMux()
	proxyOpts := []grpc.DialOption{grpc.WithInsecure()}
	err = looprpc.RegisterSwapClientHandlerFromEndpoint(
		ctx, mux, config.RPCListen, proxyOpts,
	)
	if err != nil {
		return err
	}

	logger.Infof("Starting REST proxy listener")
	restListener, err := net.Listen("tcp", config.RESTListen)
	if err != nil {
		return fmt.Errorf("REST proxy unable to listen on %s",
			config.RESTListen)
	}
	defer restListener.Close()
	proxy := &http.Server{Handler: mux}
	go proxy.Serve(restListener)

	statusChan := make(chan loop.SwapInfo)

	mainCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// Start the swap client itself.
	wg.Add(1)
	go func() {
		defer wg.Done()

		logger.Infof("Starting swap client")
		err := swapClient.Run(mainCtx, statusChan)
		if err != nil {
			logger.Error(err)
		}
		logger.Infof("Swap client stopped")

		logger.Infof("Stopping gRPC server")
		grpcServer.Stop()

		cancel()
	}()

	// Start a goroutine that broadcasts swap updates to clients.
	wg.Add(1)
	go func() {
		defer wg.Done()

		logger.Infof("Waiting for updates")
		for {
			select {
			case swap := <-statusChan:
				swapsLock.Lock()
				swaps[swap.SwapHash] = swap

				for _, subscriber := range subscribers {
					select {
					case subscriber <- swap:
					case <-mainCtx.Done():
						return
					}
				}

				swapsLock.Unlock()
			case <-mainCtx.Done():
				return
			}
		}
	}()

	// Start the grpc server.
	wg.Add(1)
	go func() {
		defer wg.Done()

		logger.Infof("RPC server listening on %s", grpcListener.Addr())
		logger.Infof("REST proxy listening on %s", restListener.Addr())

		err = grpcServer.Serve(grpcListener)
		if err != nil {
			logger.Error(err)
		}
	}()

	interruptChannel := make(chan os.Signal, 1)
	signal.Notify(interruptChannel, os.Interrupt)

	// Run until the users terminates loopd or an error occurred.
	select {
	case <-interruptChannel:
		logger.Infof("Received SIGINT (Ctrl+C).")

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
