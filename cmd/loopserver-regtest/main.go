// loopserver-regtest is a disposable, source-available Loop server that moves
// real regtest Bitcoin and pays real regtest Lightning invoices. It must never
// be used on testnet, signet or mainnet.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/lightninglabs/lndclient"
	regtestserver "github.com/lightninglabs/loop/regtest/server"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

type config struct {
	listen      string
	tlsCertPath string
	tlsKeyPath  string

	lndHost         string
	lndMacaroonPath string
	lndTLSPath      string

	bitcoinHost     string
	bitcoinUser     string
	bitcoinPassword string

	minAmount int64
	maxAmount int64
	timeout   time.Duration
}

func parseConfig() config {
	var cfg config

	flag.StringVar(&cfg.listen, "listen", "0.0.0.0:11009",
		"address for the regtest-only gRPC server")
	flag.StringVar(&cfg.tlsCertPath, "tls.certpath", "",
		"optional path to the server TLS certificate")
	flag.StringVar(&cfg.tlsKeyPath, "tls.keypath", "",
		"optional path to the server TLS private key")
	flag.StringVar(&cfg.lndHost, "lnd.host", "localhost:10009",
		"server-side lnd RPC address")
	flag.StringVar(&cfg.lndMacaroonPath, "lnd.macaroonpath", "",
		"path to the server-side lnd admin macaroon")
	flag.StringVar(&cfg.lndTLSPath, "lnd.tlspath", "",
		"path to the server-side lnd TLS certificate")
	flag.StringVar(&cfg.bitcoinHost, "bitcoin.host", "localhost:18443",
		"Bitcoin Core RPC address")
	flag.StringVar(&cfg.bitcoinUser, "bitcoin.user", "lightning",
		"Bitcoin Core RPC user")
	flag.StringVar(&cfg.bitcoinPassword, "bitcoin.password", "lightning",
		"Bitcoin Core RPC password")
	flag.Int64Var(&cfg.minAmount, "minamt", 50_000,
		"minimum swap amount in satoshis")
	flag.Int64Var(&cfg.maxAmount, "maxamt", 5_000_000,
		"maximum swap amount in satoshis")
	flag.DurationVar(&cfg.timeout, "paymenttimeout", time.Minute,
		"maximum time for a regtest Lightning payment")
	flag.Parse()

	return cfg
}

func run(ctx context.Context, cfg config) error {
	// These are the oldest APIs needed by the regtest server: MuSig2 signer,
	// wallet kit, chain notifier, router and invoices. Keeping the explicit
	// floor also allows the repository's v0.18.5 regtest image to be used.
	minLndVersion := &verrpc.Version{
		AppMajor: 0,
		AppMinor: 18,
		AppPatch: 4,
		BuildTags: []string{
			"signrpc", "walletrpc", "chainrpc", "invoicesrpc",
		},
	}

	lnd, err := lndclient.NewLndServices(&lndclient.LndServicesConfig{
		LndAddress:         cfg.lndHost,
		Network:            lndclient.NetworkRegtest,
		CustomMacaroonPath: cfg.lndMacaroonPath,
		TLSPath:            cfg.lndTLSPath,
		CheckVersion:       minLndVersion,
		CallerCtx:          ctx,
		RPCTimeout:         30 * time.Second,

		BlockUntilChainSynced:   true,
		BlockUntilUnlocked:      true,
		BlockUntilChainNotifier: true,
	})
	if err != nil {
		return fmt.Errorf("connect to lnd: %w", err)
	}
	defer lnd.Close()

	bitcoin, err := rpcclient.New(&rpcclient.ConnConfig{
		Host:         cfg.bitcoinHost,
		User:         cfg.bitcoinUser,
		Pass:         cfg.bitcoinPassword,
		Params:       "regtest",
		DisableTLS:   true,
		HTTPPostMode: true,
	}, nil)
	if err != nil {
		return fmt.Errorf("connect to bitcoind: %w", err)
	}
	defer bitcoin.Shutdown()

	chainInfo, err := bitcoin.GetBlockChainInfo()
	if err != nil {
		return fmt.Errorf("query bitcoind: %w", err)
	}
	if chainInfo.Chain != "regtest" {
		return fmt.Errorf("refusing Bitcoin network %q", chainInfo.Chain)
	}

	serverCfg := regtestserver.Config{
		Lnd:            &lnd.LndServices,
		Bitcoin:        bitcoin,
		MinSwapAmount:  btcutil.Amount(cfg.minAmount),
		MaxSwapAmount:  btcutil.Amount(cfg.maxAmount),
		PaymentTimeout: cfg.timeout,
	}
	loopServer, err := regtestserver.New(ctx, serverCfg)
	if err != nil {
		return err
	}
	defer loopServer.Stop()

	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.listen, err)
	}

	var serverOpts []grpc.ServerOption
	switch {
	case cfg.tlsCertPath == "" && cfg.tlsKeyPath == "":

	case cfg.tlsCertPath == "" || cfg.tlsKeyPath == "":
		return fmt.Errorf("both TLS certificate and key paths are required")

	default:
		creds, err := credentials.NewServerTLSFromFile(
			cfg.tlsCertPath, cfg.tlsKeyPath,
		)
		if err != nil {
			return fmt.Errorf("load server TLS credentials: %w", err)
		}

		serverOpts = append(serverOpts, grpc.Creds(creds))
	}

	grpcServer := grpc.NewServer(serverOpts...)
	swapserverrpc.RegisterSwapServerServer(grpcServer, loopServer)
	swapserverrpc.RegisterStaticAddressServerServer(grpcServer, loopServer)

	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)

	errChan := make(chan error, 1)
	go func() {
		errChan <- grpcServer.Serve(listener)
	}()

	log.Printf("regtest Loop server listening on %s", cfg.listen)
	select {
	case err := <-errChan:
		return err

	case <-ctx.Done():
		healthServer.Shutdown()
		grpcServer.GracefulStop()
		return nil
	}
}

func main() {
	cfg := parseConfig()
	ctx, cancel := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)

	err := run(ctx, cfg)
	cancel()
	if err != nil {
		log.Fatalf("loopserver-regtest: %v", err)
	}
}
