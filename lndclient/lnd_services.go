package lndclient

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lncfg"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var rpcTimeout = 30 * time.Second

// LndServices constitutes a set of required services.
type LndServices struct {
	Client        LightningClient
	WalletKit     WalletKitClient
	ChainNotifier ChainNotifierClient
	Signer        SignerClient
	Invoices      InvoicesClient
	Router        RouterClient

	ChainParams *chaincfg.Params

	macaroons *macaroonPouch
}

// GrpcLndServices constitutes a set of required RPC services.
type GrpcLndServices struct {
	LndServices

	cleanup func()
}

// NewLndServices creates a set of required RPC services.
func NewLndServices(lndAddress, application, network, macaroonDir,
	tlsPath string) (*GrpcLndServices, error) {

	// Based on the network, if the macaroon directory isn't set, then
	// we'll use the expected default locations.
	if macaroonDir == "" {
		switch network {
		case "testnet":
			macaroonDir = filepath.Join(
				defaultLndDir, defaultDataDir,
				defaultChainSubDir, "bitcoin", "testnet",
			)

		case "mainnet":
			macaroonDir = filepath.Join(
				defaultLndDir, defaultDataDir,
				defaultChainSubDir, "bitcoin", "mainnet",
			)

		case "simnet":
			macaroonDir = filepath.Join(
				defaultLndDir, defaultDataDir,
				defaultChainSubDir, "bitcoin", "simnet",
			)

		case "regtest":
			macaroonDir = filepath.Join(
				defaultLndDir, defaultDataDir,
				defaultChainSubDir, "bitcoin", "regtest",
			)

		default:
			return nil, fmt.Errorf("unsupported network: %v",
				network)
		}
	}

	// Now that we've ensured our macaroon directory is set properly, we
	// can retrieve our full macaroon pouch from the directory.
	macaroons, err := newMacaroonPouch(macaroonDir)
	if err != nil {
		return nil, fmt.Errorf("unable to obtain macaroons: %v", err)
	}

	// Setup connection with lnd
	logger.Infof("Creating lnd connection to %v", lndAddress)
	conn, err := getClientConn(lndAddress, network, tlsPath)
	if err != nil {
		return nil, err
	}

	logger.Infof("Connected to lnd")

	chainParams, err := swap.ChainParamsFromNetwork(network)
	if err != nil {
		return nil, err
	}

	lightningClient := newLightningClient(
		conn, chainParams, macaroons.adminMac,
	)

	// With our macaroons obtained, we'll ensure that the network for lnd
	// matches our expected network.
	info, err := lightningClient.GetInfo(context.Background())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("unable to get info for lnd "+
			"node: %v", err)
	}
	if network != info.Network {
		conn.Close()
		return nil, errors.New(
			"network mismatch with connected lnd instance",
		)
	}

	// With the network check passed, we'll now initialize the rest of the
	// sub-server connections, giving each of them their specific macaroon.
	notifierClient := newChainNotifierClient(conn, macaroons.chainMac)
	signerClient := newSignerClient(conn, macaroons.signerMac)
	walletKitClient := newWalletKitClient(conn, macaroons.walletKitMac)
	invoicesClient := newInvoicesClient(conn, macaroons.invoiceMac)
	routerClient := newRouterClient(conn, macaroons.routerMac)

	cleanup := func() {
		logger.Debugf("Closing lnd connection")
		conn.Close()

		logger.Debugf("Wait for client to finish")
		lightningClient.WaitForFinished()

		logger.Debugf("Wait for chain notifier to finish")
		notifierClient.WaitForFinished()

		logger.Debugf("Wait for invoices to finish")
		invoicesClient.WaitForFinished()

		logger.Debugf("Lnd services finished")
	}

	services := &GrpcLndServices{
		LndServices: LndServices{
			Client:        lightningClient,
			WalletKit:     walletKitClient,
			ChainNotifier: notifierClient,
			Signer:        signerClient,
			Invoices:      invoicesClient,
			Router:        routerClient,
			ChainParams:   chainParams,
			macaroons:     macaroons,
		},
		cleanup: cleanup,
	}

	logger.Infof("Using network %v", network)

	return services, nil
}

// Close closes the lnd connection and waits for all sub server clients to
// finish their goroutines.
func (s *GrpcLndServices) Close() {
	s.cleanup()

	logger.Debugf("Lnd services finished")
}

var (
	defaultRPCPort         = "10009"
	defaultLndDir          = btcutil.AppDataDir("lnd", false)
	defaultTLSCertFilename = "tls.cert"
	defaultTLSCertPath     = filepath.Join(
		defaultLndDir, defaultTLSCertFilename,
	)
	defaultDataDir     = "data"
	defaultChainSubDir = "chain"

	defaultAdminMacaroonFilename     = "admin.macaroon"
	defaultInvoiceMacaroonFilename   = "invoices.macaroon"
	defaultChainMacaroonFilename     = "chainnotifier.macaroon"
	defaultWalletKitMacaroonFilename = "walletkit.macaroon"
	defaultRouterMacaroonFilename    = "router.macaroon"
	defaultSignerFilename            = "signer.macaroon"

	// maxMsgRecvSize is the largest gRPC message our client will receive.
	// We set this to ~50Mb.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 50)
)

func getClientConn(address string, network string, tlsPath string) (
	*grpc.ClientConn, error) {

	// Load the specified TLS certificate and build transport credentials
	// with it.
	if tlsPath == "" {
		tlsPath = defaultTLSCertPath
	}

	creds, err := credentials.NewClientTLSFromFile(tlsPath, "")
	if err != nil {
		return nil, err
	}

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	// We need to use a custom dialer so we can also connect to unix sockets
	// and not just TCP addresses.
	opts = append(
		opts, grpc.WithDialer(
			lncfg.ClientAddressDialer(defaultRPCPort),
		),
	)
	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v", err)
	}

	return conn, nil
}
