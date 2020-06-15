package lndclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

var (
	rpcTimeout = 30 * time.Second

	// minimalCompatibleVersion is the minimum version and build tags
	// required in lnd to get all functionality implemented in lndclient.
	// Users can provide their own, specific version if needed. If only a
	// subset of the lndclient functionality is needed, the required build
	// tags can be adjusted accordingly. This default will be used as a fall
	// back version if none is specified in the configuration.
	minimalCompatibleVersion = &verrpc.Version{
		AppMajor: 0,
		AppMinor: 10,
		AppPatch: 1,
		BuildTags: []string{
			"signrpc", "walletrpc", "chainrpc", "invoicesrpc",
		},
	}

	// ErrVersionCheckNotImplemented is the error that is returned if the
	// version RPC is not implemented in lnd. This means the version of lnd
	// is lower than v0.10.0-beta.
	ErrVersionCheckNotImplemented = errors.New("version check not " +
		"implemented, need minimum lnd version of v0.10.0-beta")

	// ErrVersionIncompatible is the error that is returned if the connected
	// lnd instance is not supported.
	ErrVersionIncompatible = errors.New("version incompatible")

	// ErrBuildTagsMissing is the error that is returned if the
	// connected lnd instance does not have all built tags activated that
	// are required.
	ErrBuildTagsMissing = errors.New("build tags missing")
)

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

	// CheckVersion is the minimum version the connected lnd node needs to
	// be in order to be compatible. The node will be checked against this
	// when connecting. If no version is supplied, the default minimum
	// version will be used.
	CheckVersion *verrpc.Version

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
	Version     *verrpc.Version

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

	// Fall back to minimal compatible version if none if specified.
	if cfg.CheckVersion == nil {
		cfg.CheckVersion = minimalCompatibleVersion
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
	nodeAlias, nodeKey, version, err := checkLndCompatibility(
		conn, chainParams, readonlyMac, cfg.Network, cfg.CheckVersion,
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
			Version:       version,
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
// correct network, has the version RPC implemented, is the correct minimal
// version and supports all required build tags/subservers.
func checkLndCompatibility(conn *grpc.ClientConn, chainParams *chaincfg.Params,
	readonlyMac serializedMacaroon, network string,
	minVersion *verrpc.Version) (string, [33]byte, *verrpc.Version, error) {

	// onErr is a closure that simplifies returning multiple values in the
	// error case.
	onErr := func(err error) (string, [33]byte, *verrpc.Version, error) {
		closeErr := conn.Close()
		if closeErr != nil {
			log.Errorf("Error closing lnd connection: %v", closeErr)
		}

		// Make static error messages a bit less cryptic by adding the
		// version or build tag that we expect.
		newErr := fmt.Errorf("lnd compatibility check failed: %v", err)
		if err == ErrVersionIncompatible || err == ErrBuildTagsMissing {
			newErr = fmt.Errorf("error checking connected lnd "+
				"version. at least version \"%s\" is "+
				"required", VersionString(minVersion))
		}

		return "", [33]byte{}, nil, newErr
	}

	// We use our own clients with a readonly macaroon here, because we know
	// that's all we need for the checks.
	lightningClient := newLightningClient(conn, chainParams, readonlyMac)
	versionerClient := newVersionerClient(conn, readonlyMac)

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

	// Now let's also check the version of the connected lnd node.
	version, err := checkVersionCompatibility(versionerClient, minVersion)
	if err != nil {
		return onErr(err)
	}

	// Return the static part of the info we just queried from the node so
	// it can be cached for later use.
	return info.Alias, info.IdentityPubkey, version, nil
}

// checkVersionCompatibility makes sure the connected lnd node has the correct
// version and required build tags enabled.
//
// NOTE: This check will **never** return a non-nil error for a version of
// lnd < 0.10.0 because any version previous to 0.10.0 doesn't have the version
// endpoint implemented!
func checkVersionCompatibility(client VersionerClient,
	expected *verrpc.Version) (*verrpc.Version, error) {

	// First, test that the version RPC is even implemented.
	version, err := client.GetVersion(context.Background())
	if err != nil {
		// The version service has only been added in lnd v0.10.0. If
		// we get an unimplemented error, it means the lnd version is
		// definitely older than that.
		s, ok := status.FromError(err)
		if ok && s.Code() == codes.Unimplemented {
			return nil, ErrVersionCheckNotImplemented
		}
		return nil, fmt.Errorf("GetVersion error: %v", err)
	}

	log.Infof("lnd version: %v", VersionString(version))

	// Now check the version and make sure all required build tags are set.
	err = assertVersionCompatible(version, expected)
	if err != nil {
		return nil, err
	}
	err = assertBuildTagsEnabled(version, expected.BuildTags)
	if err != nil {
		return nil, err
	}

	// All check positive, version is fully compatible.
	return version, nil
}

// assertVersionCompatible makes sure the detected lnd version is compatible
// with our current version requirements.
func assertVersionCompatible(actual *verrpc.Version,
	expected *verrpc.Version) error {

	// We need to check the versions parts sequentially as they are
	// hierarchical.
	if actual.AppMajor != expected.AppMajor {
		if actual.AppMajor > expected.AppMajor {
			return nil
		}
		return ErrVersionIncompatible
	}

	if actual.AppMinor != expected.AppMinor {
		if actual.AppMinor > expected.AppMinor {
			return nil
		}
		return ErrVersionIncompatible
	}

	if actual.AppPatch != expected.AppPatch {
		if actual.AppPatch > expected.AppPatch {
			return nil
		}
		return ErrVersionIncompatible
	}

	// The actual version and expected version are identical.
	return nil
}

// assertBuildTagsEnabled makes sure all required build tags are set.
func assertBuildTagsEnabled(actual *verrpc.Version,
	requiredTags []string) error {

	tagMap := make(map[string]struct{})
	for _, tag := range actual.BuildTags {
		tagMap[tag] = struct{}{}
	}
	for _, required := range requiredTags {
		if _, ok := tagMap[required]; !ok {
			return ErrBuildTagsMissing
		}
	}

	// All tags found.
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
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
	}

	conn, err := grpc.Dial(cfg.LndAddress, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}
