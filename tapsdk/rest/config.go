package rest

import (
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
)

const (
	// defaultHTTPTimeout is the default timeout for HTTP
	// requests.
	defaultHTTPTimeout = 30 * time.Second
)

var (
	defaultTapdDir         = btcutil.AppDataDir("tapd", false)
	defaultTLSCertFilename = "tls.cert"
	defaultTLSCertPath     = filepath.Join(
		defaultTapdDir, defaultTLSCertFilename,
	)
	defaultDataDir     = "data"
	defaultChainSubDir = "chain"
)

// Config holds configuration for connecting to a tapd REST API.
type Config struct {
	// BaseURL is the base URL of the tapd REST API, e.g.
	// "https://localhost:8089". The scheme must be https.
	BaseURL string

	// Network is the bitcoin network we expect the tapd instance to
	// operate on. Used for resolving default macaroon paths.
	Network tapsdk.Network

	// Macaroon chooses where the SDK reads authentication
	// macaroons from. Obtain values from macaroon.FromPath,
	// macaroon.FromDir, or macaroon.FromHex. When nil, the SDK
	// falls back to tapd's default per-network directory under
	// ~/.tapd.
	Macaroon macaroon.Source

	// TLS chooses how the SDK builds its TLS trust configuration.
	// Obtain values from TLSFromPath, TLSFromData, TLSSystemCert,
	// or TLSInsecure. When nil, the SDK falls back to tapd's
	// default tls.cert path under ~/.tapd.
	TLS TLSSource

	// TLSMinVersion sets the minimum TLS version the client will accept.
	// Defaults to TLS 1.2 when zero. Use crypto/tls constants
	// (tls.VersionTLS12, tls.VersionTLS13).
	TLSMinVersion uint16

	// TLSPinnedCertFingerprint is the hex-encoded SHA-256 fingerprint
	// of the expected server certificate. When set, the client rejects
	// connections to servers presenting a different leaf certificate.
	// The fingerprint is compared against the raw DER encoding of the
	// first certificate in the peer's chain.
	TLSPinnedCertFingerprint string

	// Timeout is an optional custom timeout for HTTP requests. If
	// zero, defaults to 30 seconds.
	Timeout time.Duration
}

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
		return "", ErrUnsupportedNetwork
	}

	return filepath.Join(
		defaultTapdDir, defaultDataDir, defaultChainSubDir,
		"bitcoin", subDir,
	), nil
}

// timeout returns the configured timeout or the default.
func (c *Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}

	return defaultHTTPTimeout
}
