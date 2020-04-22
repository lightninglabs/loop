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

// NewLndServices creates creates a connection to the given lnd instance and
// creates a set of required RPC services.
func NewLndServices(lndAddress, network, macaroonDir, tlsPath string) (
	*GrpcLndServices, error) {

	// We need to use a custom dialer so we can also connect to unix
	// sockets and not just TCP addresses.
	dialer := lncfg.ClientAddressDialer(defaultRPCPort)

	return NewLndServicesWithDialer(
		dialer, lndAddress, network, macaroonDir, tlsPath,
	)
}

// NewLndServices creates a set of required RPC services by connecting to lnd
// using the given dialer.
func NewLndServicesWithDialer(dialer dialerFunc, lndAddress, network,
	macaroonDir, tlsPath string) (*GrpcLndServices, error) {

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

	// Setup connection with lnd
	log.Infof("Creating lnd connection to %v", lndAddress)
	conn, err := getClientConn(dialer, lndAddress, tlsPath)
	if err != nil {
		return nil, err
	}

	log.Infof("Connected to lnd")

	chainParams, err := swap.ChainParamsFromNetwork(network)
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
	err = checkLndCompatibility(conn, chainParams, readonlyMac, network)
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
			ChainParams:   chainParams,
			macaroons:     macaroons,
		},
		cleanup: cleanup,
	}

	log.Infof("Using network %v", network)

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
	readonlyMac serializedMacaroon, network string) error {

	// We use our own client with a readonly macaroon here, because we know
	// that's all we need for the checks.
	lightningClient := newLightningClient(conn, chainParams, readonlyMac)

	// With our readonly macaroon obtained, we'll ensure that the network
	// for lnd matches our expected network.
	info, err := lightningClient.GetInfo(context.Background())
	if err != nil {
		conn.Close()
		return fmt.Errorf("unable to get info for lnd "+
			"node: %v", err)
	}
	if network != info.Network {
		conn.Close()
		return fmt.Errorf("network mismatch with connected lnd "+
			"node, got '%s', wanted '%s'", info.Network, network)

	}
	return nil
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

type dialerFunc func(context.Context, string) (net.Conn, error)

func getClientConn(dialer dialerFunc, address string, tlsPath string) (
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

		// Use a custom dialer, to allow connections to unix sockets,
		// in-memory listeners etc, and not just TCP addresses.
		grpc.WithContextDialer(dialer),
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v", err)
	}

	return conn, nil
}
