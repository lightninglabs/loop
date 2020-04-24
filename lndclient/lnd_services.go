package lndclient

import (
	"context"
	"fmt"
	"net"
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

// LndServicesConfig holds all configuration settings that are needed to connect
// to an lnd node.
type LndServicesConfig struct {
	// LndAddress is the network address (host:port) of the lnd node to
	// connect to.
	LndAddress string

	// Network is the bitcoin network we expect the lnd node to operate on.
	Network string

	// MacaroonDir is the directory where all lnd macaroons can be found.
	MacaroonDir string

	// TLSPath is the path to lnd's TLS certificate file.
	TLSPath string

	// Dialer is an optional dial function that can be passed in if the
	// default lncfg.ClientAddressDialer should not be used.
	Dialer DialerFunc
}

// DialerFunc is a function that is used as grpc.WithContextDialer().
type DialerFunc func(context.Context, string) (net.Conn, error)

// LndServices constitutes a set of required services.
type LndServices struct {
	Client        LightningClient
	WalletKit     WalletKitClient
	ChainNotifier ChainNotifierClient
	Signer        SignerClient
	Invoices      InvoicesClient
	Router        RouterClient
	Versioner     VersionerClient

	ChainParams *chaincfg.Params
	NodeAlias   string
	NodePubkey  [33]byte

	macaroons *macaroonPouch
}

// GrpcLndServices constitutes a set of required RPC services.
type GrpcLndServices struct {
	LndServices

	cleanup func()
}

// NewLndServices creates creates a connection to the given lnd instance and
// creates a set of required RPC services.
func NewLndServices(cfg *LndServicesConfig) (*GrpcLndServices, error) {
	// We need to use a custom dialer so we can also connect to unix
	// sockets and not just TCP addresses.
	if cfg.Dialer == nil {
		cfg.Dialer = lncfg.ClientAddressDialer(defaultRPCPort)
	}

	// Based on the network, if the macaroon directory isn't set, then
	// we'll use the expected default locations.
	macaroonDir := cfg.MacaroonDir
	if macaroonDir == "" {
		switch cfg.Network {
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
				cfg.Network)
		}
	}

	// Setup connection with lnd
	log.Infof("Creating lnd connection to %v", cfg.LndAddress)
	conn, err := getClientConn(cfg)
	if err != nil {
		return nil, err
	}

	log.Infof("Connected to lnd")

	chainParams, err := swap.ChainParamsFromNetwork(cfg.Network)
	if err != nil {
		return nil, err
	}

	// We are going to check that the connected lnd is on the same network
	// and is a compatible version with all the required subservers enabled.
	// For this, we make two calls, both of which only need the readonly
	// macaroon. We don't use the pouch yet because if not all subservers
	// are enabled, then not all macaroons might be there and the user would
	// get a more cryptic error message.
	readonlyMac, err := newSerializedMacaroon(
		filepath.Join(macaroonDir, defaultReadonlyFilename),
	)
	if err != nil {
		return nil, err
	}
	nodeAlias, nodeKey, err := checkLndCompatibility(
		conn, chainParams, readonlyMac, cfg.Network,
	)
	if err != nil {
		return nil, err
	}

	// Now that we've ensured our macaroon directory is set properly, we
	// can retrieve our full macaroon pouch from the directory.
	macaroons, err := newMacaroonPouch(macaroonDir)
	if err != nil {
		return nil, fmt.Errorf("unable to obtain macaroons: %v", err)
	}

	// With the macaroons loaded and the version checked, we can now create
	// the real lightning client which uses the admin macaroon.
	lightningClient := newLightningClient(
		conn, chainParams, macaroons.adminMac,
	)

	// With the network check passed, we'll now initialize the rest of the
	// sub-server connections, giving each of them their specific macaroon.
	notifierClient := newChainNotifierClient(conn, macaroons.chainMac)
	signerClient := newSignerClient(conn, macaroons.signerMac)
	walletKitClient := newWalletKitClient(conn, macaroons.walletKitMac)
	invoicesClient := newInvoicesClient(conn, macaroons.invoiceMac)
	routerClient := newRouterClient(conn, macaroons.routerMac)
	versionerClient := newVersionerClient(conn, macaroons.readonlyMac)

	cleanup := func() {
		log.Debugf("Closing lnd connection")
		err := conn.Close()
		if err != nil {
			log.Errorf("Error closing client connection: %v", err)
		}

		log.Debugf("Wait for client to finish")
		lightningClient.WaitForFinished()

		log.Debugf("Wait for chain notifier to finish")
		notifierClient.WaitForFinished()

		log.Debugf("Wait for invoices to finish")
		invoicesClient.WaitForFinished()

		log.Debugf("Lnd services finished")
	}

	services := &GrpcLndServices{
		LndServices: LndServices{
			Client:        lightningClient,
			WalletKit:     walletKitClient,
			ChainNotifier: notifierClient,
			Signer:        signerClient,
			Invoices:      invoicesClient,
			Router:        routerClient,
			Versioner:     versionerClient,
			ChainParams:   chainParams,
			NodeAlias:     nodeAlias,
			NodePubkey:    nodeKey,
			macaroons:     macaroons,
		},
		cleanup: cleanup,
	}

	log.Infof("Using network %v", cfg.Network)

	return services, nil
}

// Close closes the lnd connection and waits for all sub server clients to
// finish their goroutines.
func (s *GrpcLndServices) Close() {
	s.cleanup()

	log.Debugf("Lnd services finished")
}

// checkLndCompatibility makes sure the connected lnd instance is running on the
// correct network.
func checkLndCompatibility(conn *grpc.ClientConn, chainParams *chaincfg.Params,
	readonlyMac serializedMacaroon, network string) (string, [33]byte,
	error) {

	// onErr is a closure that simplifies returning multiple values in the
	// error case.
	onErr := func(err error) (string, [33]byte, error) {
		closeErr := conn.Close()
		if closeErr != nil {
			log.Errorf("Error closing lnd connection: %v", closeErr)
		}

		return "", [33]byte{}, err
	}

	// We use our own client with a readonly macaroon here, because we know
	// that's all we need for the checks.
	lightningClient := newLightningClient(conn, chainParams, readonlyMac)

	// With our readonly macaroon obtained, we'll ensure that the network
	// for lnd matches our expected network.
	info, err := lightningClient.GetInfo(context.Background())
	if err != nil {
		err := fmt.Errorf("unable to get info for lnd node: %v", err)
		return onErr(err)
	}
	if network != info.Network {
		err := fmt.Errorf("network mismatch with connected lnd node, "+
			"wanted '%s', got '%s'", network, info.Network)
		return onErr(err)
	}

	// Return the static part of the info we just queried from the node so
	// it can be cached for later use.
	return info.Alias, info.IdentityPubkey, nil
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
	defaultReadonlyFilename          = "readonly.macaroon"

	// maxMsgRecvSize is the largest gRPC message our client will receive.
	// We set this to 200MiB.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)
)

func getClientConn(cfg *LndServicesConfig) (*grpc.ClientConn, error) {

	// Load the specified TLS certificate and build transport credentials
	// with it.
	tlsPath := cfg.TLSPath
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

		// Use a custom dialer, to allow connections to unix sockets,
		// in-memory listeners etc, and not just TCP addresses.
		grpc.WithContextDialer(cfg.Dialer),
	}

	conn, err := grpc.Dial(cfg.LndAddress, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}
