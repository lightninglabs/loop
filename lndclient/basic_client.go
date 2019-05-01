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

// NewBasicClient creates a new basic gRPC client to lnd. We call this client
// "basic" as it uses a global macaroon (by default the admin macaroon) for the
// entire connection, and falls back to expected defaults if the arguments
// aren't provided.
func NewBasicClient(lndHost, tlsPath, macPath, network string) (lnrpc.LightningClient, error) {
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

	if macPath == "" {
		macPath = filepath.Join(
			defaultLndDir, defaultDataDir, defaultChainSubDir,
			"bitcoin", network, defaultAdminMacaroonFilename,
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
	conn, err := grpc.Dial(lndHost, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v", err)
	}

	return lnrpc.NewLightningClient(conn), nil
}
