package grpc

import (
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
	"google.golang.org/grpc"
)

const (
	// defaultRPCTimeout is the default timeout used for rpc calls.
	defaultRPCTimeout = 30 * time.Second
)

var (
	defaultTapdDir         = btcutil.AppDataDir("tapd", false)
	defaultTLSCertFilename = "tls.cert"
	defaultTLSCertPath     = filepath.Join(
		defaultTapdDir, defaultTLSCertFilename,
	)
	defaultDataDir     = "data"
	defaultChainSubDir = "chain"

	// maxMsgRecvSize is the largest gRPC message our client will receive.
	// We set this to 800MiB.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(800 * 1024 * 1024)
)

// Config holds all configuration settings that are needed to connect to a tapd
// instance.
type Config struct {
	// Host is the network address (host:port) of the tapd instance to connect
	// to.
	Host string

	// Network is the bitcoin network we expect the tapd instance to operate on.
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

	// RPCTimeout is an optional custom timeout that will be used for rpc
	// calls to tapd. If this value is not set, it will default to 30
	// seconds.
	RPCTimeout time.Duration
}
