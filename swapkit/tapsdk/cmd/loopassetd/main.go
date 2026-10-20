package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	assetruntime "github.com/lightninglabs/loop/swapkit/tapsdk/runtime"
	"github.com/lightninglabs/loop/swapkit/tapsdk/walletsigner"
	sdk "github.com/lightninglabs/tap-sdk"
	sdkgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/tap-sdk/macaroon"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

func main() {
	flags := flag.NewFlagSet("loopassetd", flag.ContinueOnError)
	path := flags.String("config", "", "absolute runtime JSON config path")
	if flags.Parse(os.Args[1:]) != nil || *path == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr,
			"usage: loopassetd -config /path/config.json")
		os.Exit(2)
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loopassetd:", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, cfg); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "loopassetd:", err)
		os.Exit(1)
	}
}

type macaroonCredential string

func (m macaroonCredential) GetRequestMetadata(context.Context,
	...string) (map[string]string, error) {

	return map[string]string{"macaroon": string(m)}, nil
}

func (macaroonCredential) RequireTransportSecurity() bool { return true }

func tlsConfig(path string) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if path == "" {
		return config, nil
	}
	raw, err := readRegular(path, false)
	if err != nil {
		return nil, errors.New("cannot read RPC TLS certificate")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, errors.New("invalid RPC TLS certificate")
	}
	config.RootCAs = pool
	return config, nil
}

func run(ctx context.Context, cfg *config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if err := privateDirectory(cfg.StateDir); err != nil {
		return err
	}
	// Acquire the socket lease before opening either journal.
	listener, err := bridge.Listen(cfg.Socket)
	if err != nil {
		return errors.New("cannot acquire asset runtime socket")
	}
	defer listener.Close()
	token, err := readRegular(cfg.Token, true)
	if err != nil || len(token) != 32 {
		return errors.New("cannot read asset runtime token")
	}
	lndMac, err := readRegular(cfg.LND.Macaroon, true)
	if err != nil || len(lndMac) == 0 {
		return errors.New("cannot read LND macaroon")
	}
	tapMac, err := readRegular(cfg.Tapd.Macaroon, true)
	if err != nil || len(tapMac) == 0 {
		return errors.New("cannot read tapd macaroon")
	}
	lndTLS, err := tlsConfig(cfg.LND.TLSCert)
	if err != nil {
		return err
	}
	connection, err := grpc.NewClient(cfg.LND.Address,
		grpc.WithTransportCredentials(credentials.NewTLS(lndTLS)),
		grpc.WithPerRPCCredentials(macaroonCredential(
			hex.EncodeToString(lndMac))),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
	if err != nil {
		return errors.New("cannot configure LND RPC")
	}
	defer connection.Close()
	tapClient, err := sdkgrpc.NewClient(&sdkgrpc.Config{
		Host: cfg.Tapd.Address, Network: cfg.Network,
		Macaroon:      macaroon.FromHex(hex.EncodeToString(tapMac)),
		TLS:           sdkgrpc.TLSFromPath(cfg.Tapd.TLSCert),
		TLSMinVersion: tls.VersionTLS12, RPCTimeout: cfg.timeout(),
	})
	if err != nil {
		return errors.New("cannot configure tapd RPC")
	}
	defer tapClient.Close()
	raw, err := readRegular(cfg.Bitcoin.Credentials, true)
	if err != nil {
		return errors.New("cannot read Bitcoin RPC credentials")
	}
	var auth bitcoinCredentials
	if strictJSON(raw, &auth) != nil ||
		auth.Username == "" || auth.Password == "" {

		return errors.New("invalid Bitcoin RPC credentials")
	}
	chainTLS, err := tlsConfig(cfg.Bitcoin.TLSCert)
	if err != nil {
		return err
	}
	dialer := &net.Dialer{Timeout: cfg.timeout()}
	transport := &http.Transport{
		TLSClientConfig:     chainTLS,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: cfg.timeout(),
	}
	defer transport.CloseIdleConnections()
	chain := &assetruntime.RPCChain{
		URL:  cfg.Bitcoin.URL,
		User: auth.Username, Password: auth.Password,
		Client: &http.Client{
			Transport: transport, Timeout: cfg.timeout(),
			CheckRedirect: rejectRedirect,
		},
	}
	wallet := walletrpc.NewWalletKitClient(connection)
	signer, err := walletsigner.Open(
		filepath.Join(cfg.StateDir, "wallet-signing.db"),
		signrpc.NewSignerClient(connection), wallet,
	)
	if err != nil {
		return errors.New("cannot open wallet signing journal")
	}
	defer signer.Close()
	runtime, err := assetruntime.Open(assetruntime.Config{
		Path:    filepath.Join(cfg.StateDir, "runtime.db"),
		Network: cfg.Network, SDK: tapClient,
		Wallet:    sdk.NewWallet(tapClient, cfg.Network),
		Lightning: lnrpc.NewLightningClient(connection),
		WalletKit: wallet, Signer: signer, Chain: chain,
		FeeSat: cfg.FeeSat, MaxFeeSat: cfg.MaxFeeSat,
		MinConfs: cfg.MinConfs,
	})
	if err != nil {
		return errors.New("cannot open asset transaction journal")
	}
	defer runtime.Close()
	startup, cancel := context.WithTimeout(ctx, cfg.timeout())
	defer cancel()
	genesis, err := chain.Block(startup, 0)
	if err != nil || genesis == nil ||
		genesis.BlockHash() != *networkParams(cfg.Network).GenesisHash {

		return errors.New("Bitcoin backend network validation failed")
	}
	err = waitReady(startup, runtime, 250*time.Millisecond)
	if err != nil {
		return errors.New("asset wallet readiness validation failed")
	}
	handler, err := bridge.Handler(token, loggedBackend{
		Backend: runtime,
		log:     btclog.NewSLogger(btclog.NewDefaultHandler(os.Stderr)),
	})
	if err != nil {
		return errors.New("cannot configure asset runtime bridge")
	}
	return serve(ctx, listener, handler)
}

// waitReady retries temporary startup lag within the caller's fixed budget.
// Identity and network failures return immediately without exposing the socket.
func waitReady(ctx context.Context, backend bridge.Backend,
	interval time.Duration) error {

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := backend.Call(ctx, bridge.CheckReady, nil)
		if err == nil {
			return nil
		}
		code := status.Code(err)
		if !errors.Is(err, bridge.ErrPending) &&
			code != codes.Unavailable &&
			code != codes.DeadlineExceeded {

			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type loggedBackend struct {
	bridge.Backend
	log btclog.Logger
}

func (b loggedBackend) Call(ctx context.Context, method bridge.Method,
	payload json.RawMessage) (any, error) {

	result, err := b.Backend.Call(ctx, method, payload)
	if err != nil && !errors.Is(err, bridge.ErrPending) &&
		!errors.Is(err, context.Canceled) {

		// Omit payloads, nonce journals and credentials.
		b.log.ErrorS(ctx, "Asset runtime operation failed", err,
			"method", string(method))
	}
	return result, err
}

// serve cancels every action before joining handlers and closing journals.
// Shutdown has a grace deadline; Close then interrupts remaining connections.
func serve(ctx context.Context, listener net.Listener,
	handler http.Handler) error {

	actions, cancel := context.WithCancel(ctx)
	defer cancel()
	var active sync.WaitGroup
	var entry sync.Mutex
	closing := false
	stopEntry := func() {
		entry.Lock()
		closing = true
		entry.Unlock()
	}
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       35 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       30 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context {
			return actions
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter,
			r *http.Request) {

			entry.Lock()
			if closing {
				entry.Unlock()
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			active.Add(1)
			entry.Unlock()
			defer active.Done()
			handler.ServeHTTP(w, r)
		}),
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
	case err := <-done:
		cancel()
		stopEntry()
		_ = server.Close()
		active.Wait()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("asset runtime listener failed")
	}
	cancel()
	stopEntry()
	shutdown, release := context.WithTimeout(context.WithoutCancel(ctx),
		5*time.Second)
	defer release()
	if server.Shutdown(shutdown) != nil {
		_ = server.Close()
	}
	<-done
	active.Wait()
	return nil
}

func rejectRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}
