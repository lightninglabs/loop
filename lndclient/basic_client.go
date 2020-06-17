package lndclient

import (
	"fmt"
	"io/ioutil"
	"path/filepath"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	macaroon "gopkg.in/macaroon.v2"
)

// BasicClientOption is a functional option argument that allows adding arbitrary
// lnd basic client configuration overrides, without forcing existing users of
// NewBasicClient to update their invocation. These are always processed in
// order, with later options overriding earlier ones.
type BasicClientOption func(*basicClientOptions)

// basicClientOptions is a set of options that can configure the lnd client
// returned by NewBasicClient.
type basicClientOptions struct {
	macFilename string
}

// defaultBasicClientOptions returns a basicClientOptions set to lnd basic client
// defaults.
func defaultBasicClientOptions() *basicClientOptions {
	return &basicClientOptions{
		macFilename: defaultAdminMacaroonFilename,
	}
}

// MacFilename is a basic client option that sets the name of the macaroon file
// to use.
func MacFilename(macFilename string) BasicClientOption {
	return func(bc *basicClientOptions) {
		bc.macFilename = macFilename
	}
}

// applyBasicClientOptions updates a basicClientOptions set with functional
// options.
func (bc *basicClientOptions) applyBasicClientOptions(options ...BasicClientOption) {
	for _, option := range options {
		option(bc)
	}
}

// NewBasicClient creates a new basic gRPC client to lnd. We call this client
// "basic" as it falls back to expected defaults if the arguments aren't
// provided.
func NewBasicClient(lndHost, tlsPath, macDir, network string,
	basicOptions ...BasicClientOption) (

	lnrpc.LightningClient, error) {

	conn, err := NewBasicConn(
		lndHost, tlsPath, macDir, network, basicOptions...,
	)
	if err != nil {
		return nil, err
	}

	return lnrpc.NewLightningClient(conn), nil
}

// NewBasicConn creates a new basic gRPC connection to lnd. We call this
// connection "basic" as it falls back to expected defaults if the arguments
// aren't provided.
func NewBasicConn(lndHost, tlsPath, macDir, network string,
	basicOptions ...BasicClientOption) (

	*grpc.ClientConn, error) {

	if tlsPath == "" {
		tlsPath = defaultTLSCertPath
	}

	// Load the specified TLS certificate and build transport credentials
	creds, err := credentials.NewClientTLSFromFile(tlsPath, "")
	if err != nil {
		return nil, err
	}

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	if macDir == "" {
		macDir = filepath.Join(
			defaultLndDir, defaultDataDir, defaultChainSubDir,
			"bitcoin", network,
		)
	}

	// Starting with the set of default options, we'll apply any specified
	// functional options to the basic client.
	bco := defaultBasicClientOptions()
	bco.applyBasicClientOptions(basicOptions...)

	macPath := filepath.Join(macDir, bco.macFilename)

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
		opts = append(opts, grpc.WithDefaultCallOptions(maxMsgRecvSize))
	}

	// We need to use a custom dialer so we can also connect to unix sockets
	// and not just TCP addresses.
	opts = append(
		opts, grpc.WithContextDialer(
			lncfg.ClientAddressDialer(defaultRPCPort),
		),
	)
	conn, err := grpc.Dial(lndHost, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v", err)
	}

	return conn, nil
}
