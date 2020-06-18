package loopd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
)

var (
	// maxMsgRecvSize is the largest message our REST proxy will receive. We
	// set this to 200MiB atm.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)

	// errOnlyStartOnce is the error that is returned if the daemon is
	// started more than once.
	errOnlyStartOnce = fmt.Errorf("daemon can only be started once")
)

// listenerCfg holds closures used to retrieve listeners for the gRPC services.
type listenerCfg struct {
	// grpcListener returns a listener to use for the gRPC server.
	grpcListener func() (net.Listener, error)

	// restListener returns a listener to use for the REST proxy.
	restListener func() (net.Listener, error)

	// getLnd returns a grpc connection to an lnd instance.
	getLnd func(lndclient.Network, *lndConfig) (*lndclient.GrpcLndServices,
		error)
}

// Daemon is the struct that holds one instance of the loop client daemon.
type Daemon struct {
	// To be used atomically. Declared first to optimize struct alignment.
	started int32

	// swapClientServer is the embedded RPC server that satisfies the client
	// RPC interface. We embed this struct so the Daemon itself can be
	// registered to an existing grpc.Server to run as a subserver in the
	// same process.
	swapClientServer

	// ErrChan is an error channel that users of the Daemon struct must use
	// to detect runtime errors and also whether a shutdown is fully
	// completed.
	ErrChan chan error

	cfg             *Config
	listenerCfg     *listenerCfg
	internalErrChan chan error

	lnd           *lndclient.GrpcLndServices
	clientCleanup func()

	wg       sync.WaitGroup
	quit     chan struct{}
	stopOnce sync.Once

	mainCtx       context.Context
	mainCtxCancel func()

	grpcServer    *grpc.Server
	grpcListener  net.Listener
	restServer    *http.Server
	restListener  net.Listener
	restCtxCancel func()
}

// New creates a new instance of the loop client daemon.
func New(config *Config, lisCfg *listenerCfg) *Daemon {
	return &Daemon{
		// We send exactly one error on this channel if something goes
		// wrong at runtime. Or a nil value if the shutdown was
		// successful. But in case nobody's listening, we don't want to
		// block on it so we buffer it.
		ErrChan: make(chan error, 1),

		quit:        make(chan struct{}),
		cfg:         config,
		listenerCfg: lisCfg,

		// We have 3 goroutines that could potentially send an error.
		// We react on the first error but in case more than one exits
		// with an error we don't want them to block.
		internalErrChan: make(chan error, 3),
	}
}

// Start starts loopd in daemon mode. It will listen for grpc connections,
// execute commands and pass back swap status information.
func (d *Daemon) Start() error {
	// There should be no reason to start the daemon twice. Therefore return
	// an error if that's tried. This is mostly to guard against Start and
	// StartAsSubserver both being called.
	if atomic.AddInt32(&d.started, 1) != 1 {
		return errOnlyStartOnce
	}

	network := lndclient.Network(d.cfg.Network)

	var err error
	d.lnd, err = d.listenerCfg.getLnd(network, d.cfg.Lnd)
	if err != nil {
		return err
	}

	// With lnd connected, initialize everything else, such as the swap
	// server client, the swap client RPC server instance and our main swap
	// and error handlers. If this fails, then nothing has been started yet
	// and we can just return the error.
	err = d.initialize()
	if err != nil {
		return err
	}

	// If we get here, we already have started several goroutines. So if
	// anything goes wrong now, we need to cleanly shut down again.
	startErr := d.startWebServers()
	if startErr != nil {
		log.Errorf("Error while starting daemon: %v", err)
		d.Stop()
		stopErr := <-d.ErrChan
		if stopErr != nil {
			log.Errorf("Error while stopping daemon: %v", stopErr)
		}
		return startErr
	}

	return nil
}

// StartAsSubserver is an alternative to Start where the RPC server does not
// create its own gRPC server but registers to an existing one. The same goes
// for REST (if enabled), instead of creating an own mux and HTTP server, we
// register to an existing one.
func (d *Daemon) StartAsSubserver(lndGrpc *lndclient.GrpcLndServices) error {
	// There should be no reason to start the daemon twice. Therefore return
	// an error if that's tried. This is mostly to guard against Start and
	// StartAsSubserver both being called.
	if atomic.AddInt32(&d.started, 1) != 1 {
		return errOnlyStartOnce
	}

	// When starting as a subserver, we get passed in an already established
	// connection to lnd that might be shared among other subservers.
	d.lnd = lndGrpc

	// With lnd already pre-connected, initialize everything else, such as
	// the swap server client, the RPC server instance and our main swap
	// handlers. If this fails, then nothing has been started yet and we can
	// just return the error.
	return d.initialize()
}

// startWebServers starts the gRPC and REST servers in goroutines.
func (d *Daemon) startWebServers() error {
	var err error

	// With our client created, let's now finish setting up and start our
	// RPC server.
	serverOpts := []grpc.ServerOption{}
	d.grpcServer = grpc.NewServer(serverOpts...)
	looprpc.RegisterSwapClientServer(d.grpcServer, d)

	// Next, start the gRPC server listening for HTTP/2 connections.
	log.Infof("Starting gRPC listener")
	d.grpcListener, err = d.listenerCfg.grpcListener()
	if err != nil {
		return fmt.Errorf("RPC server unable to listen on %s: %v",
			d.cfg.RPCListen, err)

	}

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
	d.restCtxCancel = cancel
	mux := proxy.NewServeMux(customMarshalerOption)
	var restHandler http.Handler = mux
	if d.cfg.CORSOrigin != "" {
		restHandler = allowCORS(restHandler, d.cfg.CORSOrigin)
	}
	proxyOpts := []grpc.DialOption{
		grpc.WithInsecure(),
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
	}
	err = looprpc.RegisterSwapClientHandlerFromEndpoint(
		ctx, mux, d.cfg.RPCListen, proxyOpts,
	)
	if err != nil {
		return err
	}

	d.restListener, err = d.listenerCfg.restListener()
	if err != nil {
		return fmt.Errorf("REST proxy unable to listen on %s: %v",
			d.cfg.RESTListen, err)
	}

	// A nil listener indicates REST is disabled.
	if d.restListener != nil {
		log.Infof("Starting REST proxy listener")

		d.restServer = &http.Server{Handler: restHandler}

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()

			log.Infof("REST proxy listening on %s",
				d.restListener.Addr())
			err := d.restServer.Serve(d.restListener)
			// ErrServerClosed is always returned when the proxy is
			// shut down, so don't log it.
			if err != nil && err != http.ErrServerClosed {
				// Notify the main error handler goroutine that
				// we exited unexpectedly here. We don't have to
				// worry about blocking as the internal error
				// channel is sufficiently buffered.
				d.internalErrChan <- err
			}
		}()
	} else {
		log.Infof("REST proxy disabled")
	}

	// Start the grpc server.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()

		log.Infof("RPC server listening on %s", d.grpcListener.Addr())
		err = d.grpcServer.Serve(d.grpcListener)
		if err != nil && err != grpc.ErrServerStopped {
			// Notify the main error handler goroutine that
			// we exited unexpectedly here. We don't have to
			// worry about blocking as the internal error
			// channel is sufficiently buffered.
			d.internalErrChan <- err
		}
	}()

	return nil
}

// initialize creates and initializes an instance of the swap server client,
// the swap client RPC server instance and our main swap and error handlers. If
// this method fails with an error then no goroutine was started yet and no
// cleanup is necessary. If it succeeds, then goroutines have been spawned.
func (d *Daemon) initialize() error {
	// If no swap server is specified, use the default addresses for mainnet
	// and testnet.
	if d.cfg.Server.Host == "" {
		// TODO(wilmer): Use onion service addresses when proxy is
		// active.
		switch d.cfg.Network {
		case "mainnet":
			d.cfg.Server.Host = mainnetServer
		case "testnet":
			d.cfg.Server.Host = testnetServer
		default:
			return errors.New("no swap server address specified")
		}
	}

	log.Infof("Swap server address: %v", d.cfg.Server.Host)

	// Create an instance of the loop client library.
	swapclient, clientCleanup, err := getClient(d.cfg, &d.lnd.LndServices)
	if err != nil {
		return err
	}
	d.clientCleanup = clientCleanup

	// Both the client RPC server and and the swap server client should
	// stop on main context cancel. So we create it early and pass it down.
	d.mainCtx, d.mainCtxCancel = context.WithCancel(context.Background())

	// Now finally fully initialize the swap client RPC server instance.
	d.swapClientServer = swapClientServer{
		impl:        swapclient,
		lnd:         &d.lnd.LndServices,
		swaps:       make(map[lntypes.Hash]loop.SwapInfo),
		subscribers: make(map[int]chan<- interface{}),
		statusChan:  make(chan loop.SwapInfo),
		mainCtx:     d.mainCtx,
	}

	// Retrieve all currently existing swaps from the database.
	swapsList, err := d.impl.FetchSwaps()
	if err != nil {
		// The client is the only thing we started yet, so if we clean
		// up its connection now, nothing else needs to be shut down at
		// this point.
		clientCleanup()
		return err
	}

	for _, s := range swapsList {
		d.swaps[s.SwapHash] = *s
	}

	// Start the swap client itself.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()

		log.Infof("Starting swap client")
		err := d.impl.Run(d.mainCtx, d.statusChan)
		if err != nil {
			// Notify the main error handler goroutine that
			// we exited unexpectedly here. We don't have to
			// worry about blocking as the internal error
			// channel is sufficiently buffered.
			d.internalErrChan <- err
		}
		log.Infof("Swap client stopped")
	}()

	// Start a goroutine that broadcasts swap updates to clients.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()

		log.Infof("Waiting for updates")
		d.processStatusUpdates(d.mainCtx)
	}()

	// Last, start our internal error handler. This will return exactly one
	// error or nil on the main error channel to inform the caller that
	// something went wrong or that shutdown is complete. We don't add to
	// the wait group here because this goroutine will itself wait for the
	// stop to complete and signal its completion through the main error
	// channel.
	go func() {
		var runtimeErr error

		// There are only two ways this goroutine can exit. Either there
		// is an internal error or the caller requests shutdown. In both
		// cases we wait for the stop to complete before we signal the
		// caller that we're done.
		select {
		case runtimeErr = <-d.internalErrChan:
			log.Errorf("Runtime error in daemon, shutting down: "+
				"%v", runtimeErr)

		case <-d.quit:
		}

		// We need to shutdown before sending the error on the channel,
		// otherwise a caller might exit the process too early.
		d.stop()
		log.Info("Daemon exited")

		// The caller expects exactly one message. So we send the error
		// even if it's nil because we cleanly shut down.
		d.ErrChan <- runtimeErr
	}()

	return nil
}

// Stop tries to gracefully shut down the daemon. A caller needs to wait for a
// message on the main error channel indicating that the shutdown is completed.
func (d *Daemon) Stop() {
	d.stopOnce.Do(func() {
		close(d.quit)
	})
}

// stop does the actual shutdown and blocks until all goroutines have exit.
func (d *Daemon) stop() {
	// First of all, we can cancel the main context that all event handlers
	// are using. This should stop all swap activity and all event handlers
	// should exit.
	if d.mainCtxCancel != nil {
		d.mainCtxCancel()
	}

	// As there is no swap activity anymore, we can forcefully shutdown the
	// gRPC and HTTP servers now.
	log.Infof("Stopping gRPC server")
	if d.grpcServer != nil {
		d.grpcServer.Stop()
	}
	log.Infof("Stopping REST server")
	if d.restServer != nil {
		// Don't return the error here, we first want to give everything
		// else a chance to shut down cleanly.
		err := d.restServer.Close()
		if err != nil {
			log.Errorf("Error stopping REST server: %v", err)
		}
	}
	if d.restCtxCancel != nil {
		d.restCtxCancel()
	}

	// Next, shut down the connections to lnd and the swap server.
	if d.lnd != nil {
		d.lnd.Close()
	}
	if d.clientCleanup != nil {
		d.clientCleanup()
	}

	// Everything should be shutting down now, wait for completion.
	d.wg.Wait()
}

// allowCORS wraps the given http.Handler with a function that adds the
// Access-Control-Allow-Origin header to the response.
func allowCORS(handler http.Handler, origin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		handler.ServeHTTP(w, r)
	})
}
