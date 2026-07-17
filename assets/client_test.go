package assets

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"gopkg.in/macaroon.v2"
)

// TestDefaultTapdConfig tests that the default tapd connection paths match
// tapd's mainnet defaults.
func TestDefaultTapdConfig(t *testing.T) {
	defaultTapdDir := btcutil.AppDataDir("tapd", false)
	config := DefaultTapdConfig()

	require.Equal(t, filepath.Join(
		defaultTapdDir, "data", "mainnet", "admin.macaroon",
	), config.MacaroonPath)
	require.Equal(
		t, filepath.Join(defaultTapdDir, "tls.cert"), config.TLSPath,
	)
}

// TestTapdConfigClientConn tests that the default tapd file layout can be used
// to construct a client connection.
func TestTapdConfigClientConn(t *testing.T) {
	// Use an isolated tapd root so the test never reads from or writes to a
	// user's real tapd data directory.
	defaultTapdDir := t.TempDir()
	network := "regtest"
	macaroonPath := filepath.Join(
		defaultTapdDir, "data", network, "admin.macaroon",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(macaroonPath), 0o700))

	// NewTapdClient parses the configured TLS certificate before creating
	// its gRPC client. An httptest server provides a valid certificate
	// without requiring a running tapd instance.
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(tlsServer.Close)
	cert := tlsServer.Certificate()
	certBytes := pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: cert.Raw,
	})
	require.NoError(t, os.WriteFile(
		filepath.Join(defaultTapdDir, "tls.cert"), certBytes, 0o600,
	))

	// Store a valid serialized macaroon at tapd's production path. This
	// ensures connection setup tests the path itself rather than failing on
	// malformed credentials.
	mac, err := macaroon.New(
		[]byte("root-key"), []byte("id"), "tapd",
		macaroon.LatestVersion,
	)
	require.NoError(t, err)
	macBytes, err := mac.MarshalBinary()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(macaroonPath, macBytes, 0o600))

	// grpc.NewClient connects lazily, so constructing the tapd client verifies
	// that both credentials can be loaded and parsed without needing a live
	// tapd server.
	config := DefaultTapdConfigForNetwork(defaultTapdDir, network)
	client, err := NewTapdClient(config)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.cc.Close())
	})

	require.Equal(t, macaroonPath, config.MacaroonPath)
	require.Equal(
		t, filepath.Join(defaultTapdDir, "tls.cert"), config.TLSPath,
	)
}

func TestGetPaymentMaxAmount(t *testing.T) {
	tests := []struct {
		satAmount          btcutil.Amount
		feeLimitMultiplier float64
		expectedAmount     lnwire.MilliSatoshi
		expectError        bool
	}{
		{
			satAmount:          btcutil.Amount(250000),
			feeLimitMultiplier: 1.2,
			expectedAmount:     lnwire.MilliSatoshi(300000000),
			expectError:        false,
		},
		{
			satAmount:          btcutil.Amount(100000),
			feeLimitMultiplier: 1.5,
			expectedAmount:     lnwire.MilliSatoshi(150000000),
			expectError:        false,
		},
		{
			satAmount:          btcutil.Amount(50000),
			feeLimitMultiplier: 2.0,
			expectedAmount:     lnwire.MilliSatoshi(100000000),
			expectError:        false,
		},
		{
			satAmount:          btcutil.Amount(0),
			feeLimitMultiplier: 1.2,
			expectedAmount:     lnwire.MilliSatoshi(0),
			expectError:        true,
		},
		{
			satAmount:          btcutil.Amount(250000),
			feeLimitMultiplier: 0.8,
			expectedAmount:     lnwire.MilliSatoshi(0),
			expectError:        true,
		},
	}

	for _, test := range tests {
		result, err := getPaymentMaxAmount(
			test.satAmount, test.feeLimitMultiplier,
		)
		if test.expectError {
			if err == nil {
				t.Fatalf("expected error but got none")
			}
		} else {
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != test.expectedAmount {
				t.Fatalf("expected %v, got %v",
					test.expectedAmount, result)
			}
		}
	}
}

func TestGetSatsFromAssetAmt(t *testing.T) {
	tests := []struct {
		assetAmt    uint64
		assetRate   *rfqrpc.FixedPoint
		expected    btcutil.Amount
		expectError bool
	}{
		{
			assetAmt:    1000,
			assetRate:   &rfqrpc.FixedPoint{Coefficient: "100000", Scale: 0},
			expected:    btcutil.Amount(1000000),
			expectError: false,
		},
		{
			assetAmt:    500000,
			assetRate:   &rfqrpc.FixedPoint{Coefficient: "200000000", Scale: 0},
			expected:    btcutil.Amount(250000),
			expectError: false,
		},
		{
			assetAmt:    0,
			assetRate:   &rfqrpc.FixedPoint{Coefficient: "100000000", Scale: 0},
			expected:    btcutil.Amount(0),
			expectError: false,
		},
	}

	for _, test := range tests {
		result, err := getSatsFromAssetAmt(test.assetAmt, test.assetRate)
		if test.expectError {
			require.NotNil(t, err)
		} else {
			require.Nil(t, err)
			require.Equal(t, test.expected, result)
		}
	}
}
