package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/lightninglabs/loop/client"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
)

// daemon runs swapd in daemon mode. It will listen for grpc connections,
// execute commands and pass back swap status information.
func daemon(ctx *cli.Context) error {
	lnd, err := getLnd(ctx)
	if err != nil {
		return err
	}
	defer lnd.Close()

	swapClient, cleanup, err := getClient(ctx, &lnd.LndServices)
	if err != nil {
		return err
	}
	defer cleanup()

	// Before starting the client, build an in-memory view of all swaps.
	// This view is used to update newly connected clients with the most
	// recent swaps.
	storedSwaps, err := swapClient.GetUnchargeSwaps()
	if err != nil {
		return err
	}
	for _, swap := range storedSwaps {
		swaps[swap.Hash] = client.SwapInfo{
			SwapType:     client.SwapTypeUncharge,
			SwapContract: swap.Contract.SwapContract,
			State:        swap.State(),
			SwapHash:     swap.Hash,
			LastUpdate:   swap.LastUpdateTime(),
		}
	}

	// Instantiate the swapd gRPC server.
	server := swapClientServer{
		impl: swapClient,
		lnd:  &lnd.LndServices,
	}

	serverOpts := []grpc.ServerOption{}
	grpcServer := grpc.NewServer(serverOpts...)
	looprpc.RegisterSwapClientServer(grpcServer, &server)

	// Next, Start the gRPC server listening for HTTP/2 connections.
	logger.Infof("Starting RPC listener")
	lis, err := net.Listen("tcp", defaultListenAddr)
	if err != nil {
		return fmt.Errorf("RPC server unable to listen on %s",
			defaultListenAddr)

	}
	defer lis.Close()

	statusChan := make(chan client.SwapInfo)

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

		logger.Infof("RPC server listening on %s", lis.Addr())

		err = grpcServer.Serve(lis)
		if err != nil {
			logger.Error(err)
		}
	}()

	interruptChannel := make(chan os.Signal, 1)
	signal.Notify(interruptChannel, os.Interrupt)

	// Run until the users terminates swapd or an error occurred.
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
