package grpc

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// defaultMacaroonDir returns the path under ~/.tapd where tapd stores
// its per-network macaroons. Used as a fallback when Config.Macaroon
// is nil.
func defaultMacaroonDir(network tapsdk.Network) (string, error) {
	var subDir string
	switch network {
	case tapsdk.NetworkTestnet:
		subDir = "testnet"
	case tapsdk.NetworkTestnet4:
		subDir = "testnet4"
	case tapsdk.NetworkMainnet:
		subDir = "mainnet"
	case tapsdk.NetworkSimnet:
		subDir = "simnet"
	case tapsdk.NetworkSignet:
		subDir = "signet"
	case tapsdk.NetworkRegtest:
		subDir = "regtest"
	default:
		return "", fmt.Errorf("unsupported network: %v", network)
	}

	return filepath.Join(
		defaultTapdDir, defaultDataDir, defaultChainSubDir,
		"bitcoin", subDir,
	), nil
}

// Client holds the connection to the tapd daemon and the sub-clients.
type Client struct {
	*walletClient
	*walletKitClient
	*proofClient
	*universeClient
	*mintClient
	*eventClient

	grpcConn  *grpc.ClientConn
	macaroons macaroon.Pouch
}

// NewClient creates a new Client instance.
func NewClient(cfg *Config) (*Client, error) {
	macSource := cfg.Macaroon
	if macSource == nil {
		defaultDir, err := defaultMacaroonDir(cfg.Network)
		if err != nil {
			return nil, err
		}
		macSource = macaroon.FromDir(defaultDir)
	}

	conn, err := getClientConn(cfg)
	if err != nil {
		return nil, err
	}

	if cfg.RPCTimeout == 0 {
		cfg.RPCTimeout = defaultRPCTimeout
	}

	macaroons, err := macSource.LoadPouch()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("failed to read macaroons: %v", err)
	}

	client := newClient(conn, cfg.RPCTimeout, macaroons)
	client.grpcConn = conn
	return client, nil
}

// NewClientFromConn creates a client over an existing authenticated connection.
// The caller configures transport security, credentials and message limits, and
// retains ownership of the connection. Closing the client leaves it open.
// Non-positive timeouts use the default RPC timeout of 30 seconds.
func NewClientFromConn(conn grpc.ClientConnInterface,
	timeout time.Duration) *Client {

	if timeout <= 0 {
		timeout = defaultRPCTimeout
	}

	return newClient(conn, timeout, nil)
}

func newClient(conn grpc.ClientConnInterface, timeout time.Duration,
	macaroons macaroon.Pouch) *Client {

	return &Client{
		walletClient: NewWalletClient(
			conn, timeout, macaroons[macaroon.AdminServiceMac],
		),
		walletKitClient: NewWalletKitClient(
			conn, timeout, macaroons[macaroon.WalletKitServiceMac],
		),
		proofClient: NewProofClient(
			conn, timeout, macaroons[macaroon.ProofServiceMac],
		),
		universeClient: NewUniverseClient(
			conn, timeout, macaroons[macaroon.UniverseServiceMac],
		),
		mintClient: NewMintClient(
			conn, timeout, macaroons[macaroon.MintServiceMac],
		),
		eventClient: NewEventClient(
			conn, timeout, macaroons[macaroon.AdminServiceMac],
			macaroons[macaroon.MintServiceMac],
		),
		macaroons: macaroons,
	}
}

// Close tears down a connection created by NewClient. Connections supplied
// through NewClientFromConn remain owned by the caller and are left open.
func (c *Client) Close() error {
	if c.grpcConn != nil {
		return c.grpcConn.Close()
	}

	return nil
}

// getClientConn gets a client connection to the tapd host.
func getClientConn(cfg *Config) (*grpc.ClientConn, error) {
	creds, err := getTLSCredentials(cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to get tls creds: %v", err)
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
	}

	conn, err := grpc.NewClient(cfg.Host, opts...)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// defaultTLSMinVersion is the minimum TLS version the client will
// accept when no explicit version is configured.
const defaultTLSMinVersion = tls.VersionTLS12

// getTLSCredentials builds the gRPC transport credentials from the
// client configuration. It enforces a minimum TLS version floor and
// optionally pins the server certificate by SHA-256 fingerprint.
func getTLSCredentials(
	cfg *Config) (credentials.TransportCredentials, error) {

	minVersion := cfg.TLSMinVersion
	if minVersion == 0 {
		minVersion = defaultTLSMinVersion
	}

	source := cfg.TLS
	if source == nil {
		// No explicit source — fall back to tapd's default
		// tls.cert under ~/.tapd.
		source = TLSFromPath(defaultTLSCertPath)
	}

	tlsCfg, err := source.tlsConfig(minVersion)
	if err != nil {
		return nil, err
	}

	// When a pinned certificate fingerprint is provided, install a
	// VerifyConnection callback that compares the SHA-256
	// digest of the leaf certificate against the expected value.
	if cfg.TLSPinnedCertFingerprint != "" {
		expected := strings.ToLower(strings.ReplaceAll(
			cfg.TLSPinnedCertFingerprint, ":", "",
		))

		if _, err := hex.DecodeString(expected); err != nil ||
			len(expected) != 64 {

			return nil, fmt.Errorf("TLSPinnedCertFingerprint "+
				"must be a 64-char hex SHA-256 digest, "+
				"got %q", cfg.TLSPinnedCertFingerprint)
		}

		tlsCfg.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("server presented " +
					"no certificates")
			}

			digest := sha256.Sum256(state.PeerCertificates[0].Raw)
			actual := hex.EncodeToString(digest[:])

			if actual != expected {
				return fmt.Errorf("certificate "+
					"fingerprint mismatch: "+
					"got %s, want %s",
					actual, expected)
			}

			return nil
		}
	}

	return credentials.NewTLS(tlsCfg), nil
}

// certPoolFromPEM decodes a PEM-encoded certificate and returns a
// certificate pool containing it.
func certPoolFromPEM(pemData []byte) (*x509.CertPool, error) {
	block, _ := pem.Decode(pemData)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("failed to decode PEM block " +
			"containing tls certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}

	pool := x509.NewCertPool()
	pool.AddCert(cert)

	return pool, nil
}

// certPoolFromFile reads a PEM certificate file and returns a
// certificate pool.
func certPoolFromFile(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("unable to read TLS cert at %s: "+
			"%v", path, err)
	}

	return certPoolFromPEM(data)
}
