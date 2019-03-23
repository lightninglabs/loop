package lndclient

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	macaroon "gopkg.in/macaroon.v2"
)

var rpcTimeout = 30 * time.Second

// LndServices constitutes a set of required services.
type LndServices struct {
	Client        LightningClient
	WalletKit     WalletKitClient
	ChainNotifier ChainNotifierClient
	Signer        SignerClient
	Invoices      InvoicesClient

	ChainParams *chaincfg.Params
}

// GrpcLndServices constitutes a set of required RPC services.
type GrpcLndServices struct {
	LndServices

	cleanup func()
}

// NewLndServices creates a set of required RPC services.
func NewLndServices(lndAddress string, application string,
	network string, macPath, tlsPath string) (
	*GrpcLndServices, error) {

	// Setup connection with lnd
	log.Infof("Creating lnd connection to %v", lndAddress)
	conn, err := getClientConn(lndAddress, network, macPath, tlsPath)
	if err != nil {
		return nil, err
	}

	log.Infof("Connected to lnd")

	chainParams, err := swap.ChainParamsFromNetwork(network)
	if err != nil {
		return nil, err
	}

	lightningClient := newLightningClient(conn, chainParams)

	info, err := lightningClient.GetInfo(context.Background())
	if err != nil {
		conn.Close()
		return nil, err
	}
	if network != info.Network {
		conn.Close()
		return nil, errors.New(
			"network mismatch with connected lnd instance",
		)
	}

	notifierClient := newChainNotifierClient(conn)
	signerClient := newSignerClient(conn)
	walletKitClient := newWalletKitClient(conn)
	invoicesClient := newInvoicesClient(conn)

	cleanup := func() {
		log.Debugf("Closing lnd connection")
		conn.Close()

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
			ChainParams:   chainParams,
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

var (
	defaultRPCPort         = "10009"
	defaultLndDir          = btcutil.AppDataDir("lnd", false)
	defaultTLSCertFilename = "tls.cert"
	defaultTLSCertPath     = filepath.Join(defaultLndDir,
		defaultTLSCertFilename)
	defaultDataDir          = "data"
	defaultChainSubDir      = "chain"
	defaultMacaroonFilename = "admin.macaroon"
)

func getClientConn(address string, network string, macPath, tlsPath string) (
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

	if macPath == "" {
		macPath = filepath.Join(
			defaultLndDir, defaultDataDir, defaultChainSubDir,
			"bitcoin", network, defaultMacaroonFilename,
		)
	}

	// Load the specified macaroon file.
	macBytes, err := ioutil.ReadFile(macPath)
	if err == nil {
		// Only if file is found
		mac := &macaroon.Macaroon{}
		if err = mac.UnmarshalBinary(macBytes); err != nil {
			return nil, fmt.Errorf("unable to decode macaroon: %v",
				err)
		}

		// Now we append the macaroon credentials to the dial options.
		cred := macaroons.NewMacaroonCredential(mac)
		opts = append(opts, grpc.WithPerRPCCredentials(cred))
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
