package grpc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// generateSelfSignedCert creates a self-signed TLS certificate
// and returns the PEM-encoded cert and the raw DER bytes.
func generateSelfSignedCert(t *testing.T) ([]byte, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"tap-sdk-test"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(
		rand.Reader, template, template, &key.PublicKey, key,
	)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: derBytes,
	})

	return certPEM, derBytes
}

// writeTempFile writes data to a temporary file and returns its path.
func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, name)
	err := os.WriteFile(path, data, 0600)
	require.NoError(t, err)

	return path
}

// TestGetTLSCredentials_Insecure verifies that the insecure source
// returns valid credentials.
func TestGetTLSCredentials_Insecure(t *testing.T) {
	t.Parallel()

	cfg := &Config{TLS: TLSInsecure()}
	creds, err := getTLSCredentials(cfg)
	require.NoError(t, err)
	require.NotNil(t, creds)

	info := creds.Info()
	require.Equal(t, "tls", info.SecurityProtocol)
}

// TestGetTLSCredentials_SystemCert verifies the system cert pool path.
func TestGetTLSCredentials_SystemCert(t *testing.T) {
	t.Parallel()

	cfg := &Config{TLS: TLSSystemCert()}
	creds, err := getTLSCredentials(cfg)
	require.NoError(t, err)
	require.NotNil(t, creds)

	info := creds.Info()
	require.Equal(t, "tls", info.SecurityProtocol)
}

// TestGetTLSCredentials_TLSData verifies loading a certificate from
// PEM data.
func TestGetTLSCredentials_TLSData(t *testing.T) {
	t.Parallel()

	certPEM, _ := generateSelfSignedCert(t)

	cfg := &Config{TLS: TLSFromData(string(certPEM))}
	creds, err := getTLSCredentials(cfg)
	require.NoError(t, err)
	require.NotNil(t, creds)
}

// TestGetTLSCredentials_TLSPath verifies loading a certificate from a
// file path.
func TestGetTLSCredentials_TLSPath(t *testing.T) {
	t.Parallel()

	certPEM, _ := generateSelfSignedCert(t)
	certPath := writeTempFile(t, "tls.cert", certPEM)

	cfg := &Config{TLS: TLSFromPath(certPath)}
	creds, err := getTLSCredentials(cfg)
	require.NoError(t, err)
	require.NotNil(t, creds)
}

// TestGetTLSCredentials_InvalidTLSData verifies that invalid PEM data
// returns an error.
func TestGetTLSCredentials_InvalidTLSData(t *testing.T) {
	t.Parallel()

	cfg := &Config{TLS: TLSFromData("not-a-valid-pem")}
	_, err := getTLSCredentials(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to decode PEM block")
}

// TestGetTLSCredentials_MissingTLSFile verifies that a nonexistent
// TLS path returns an error.
func TestGetTLSCredentials_MissingTLSFile(t *testing.T) {
	t.Parallel()

	cfg := &Config{TLS: TLSFromPath("/nonexistent/path/tls.cert")}
	_, err := getTLSCredentials(cfg)
	require.Error(t, err)
}

// TestGetTLSCredentials_MinVersionDefault verifies the default TLS
// minimum version path builds valid credentials.
func TestGetTLSCredentials_MinVersionDefault(t *testing.T) {
	t.Parallel()

	cfg := &Config{TLS: TLSSystemCert()}
	creds, err := getTLSCredentials(cfg)
	require.NoError(t, err)
	require.NotNil(t, creds)
}

// TestGetTLSCredentials_MinVersionCustom verifies that a custom TLS
// minimum version is accepted.
func TestGetTLSCredentials_MinVersionCustom(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		TLS:           TLSSystemCert(),
		TLSMinVersion: tls.VersionTLS13,
	}
	creds, err := getTLSCredentials(cfg)
	require.NoError(t, err)
	require.NotNil(t, creds)
}

// TestGetTLSCredentials_PinnedFingerprint verifies that a valid pinned
// fingerprint is accepted during configuration.
func TestGetTLSCredentials_PinnedFingerprint(t *testing.T) {
	t.Parallel()

	certPEM, derBytes := generateSelfSignedCert(t)
	digest := sha256.Sum256(derBytes)
	fingerprint := hex.EncodeToString(digest[:])

	cfg := &Config{
		TLS:                      TLSFromData(string(certPEM)),
		TLSPinnedCertFingerprint: fingerprint,
	}
	creds, err := getTLSCredentials(cfg)
	require.NoError(t, err)
	require.NotNil(t, creds)
}

// TestGetTLSCredentials_PinnedFingerprintColons verifies that
// colon-separated fingerprints are accepted.
func TestGetTLSCredentials_PinnedFingerprintColons(t *testing.T) {
	t.Parallel()

	certPEM, derBytes := generateSelfSignedCert(t)
	digest := sha256.Sum256(derBytes)
	raw := hex.EncodeToString(digest[:])

	// Insert colons every two characters.
	var b strings.Builder
	for i, c := range raw {
		if i > 0 && i%2 == 0 {
			b.WriteRune(':')
		}
		b.WriteRune(c)
	}
	withColons := b.String()

	cfg := &Config{
		TLS:                      TLSFromData(string(certPEM)),
		TLSPinnedCertFingerprint: withColons,
	}
	creds, err := getTLSCredentials(cfg)
	require.NoError(t, err)
	require.NotNil(t, creds)
}

// TestGetTLSCredentials_InvalidPinnedFingerprint verifies that an
// invalid fingerprint format is rejected at config time.
func TestGetTLSCredentials_InvalidPinnedFingerprint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fingerprint string
	}{
		{
			name:        "too short",
			fingerprint: "abcd1234",
		},
		{
			name: "not hex",
			fingerprint: "zzzzzzzzzzzzzzzzzzzzzzzz" +
				"zzzzzzzzzzzzzzzzzzzzzzzz" +
				"zzzzzzzzzzzzzzzz",
		},
		{
			name:        "empty",
			fingerprint: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if tc.fingerprint == "" {
				// Empty fingerprint means no pinning,
				// which is valid (no-op).
				cfg := &Config{
					TLS:                      TLSSystemCert(),
					TLSPinnedCertFingerprint: tc.fingerprint,
				}
				creds, err := getTLSCredentials(cfg)
				require.NoError(t, err)
				require.NotNil(t, creds)
				return
			}

			cfg := &Config{
				TLS:                      TLSSystemCert(),
				TLSPinnedCertFingerprint: tc.fingerprint,
			}
			_, err := getTLSCredentials(cfg)
			require.Error(t, err)
			require.Contains(t, err.Error(),
				"TLSPinnedCertFingerprint")
		})
	}
}

// TestCertPoolFromPEM verifies the PEM-to-pool helper.
func TestCertPoolFromPEM(t *testing.T) {
	t.Parallel()

	certPEM, _ := generateSelfSignedCert(t)

	pool, err := certPoolFromPEM(certPEM)
	require.NoError(t, err)
	require.NotNil(t, pool)
}

// TestCertPoolFromPEM_Invalid verifies that invalid PEM is rejected.
func TestCertPoolFromPEM_Invalid(t *testing.T) {
	t.Parallel()

	_, err := certPoolFromPEM([]byte("not a pem"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to decode PEM block")
}

// TestCertPoolFromFile verifies the file-to-pool helper.
func TestCertPoolFromFile(t *testing.T) {
	t.Parallel()

	certPEM, _ := generateSelfSignedCert(t)
	path := writeTempFile(t, "tls.cert", certPEM)

	pool, err := certPoolFromFile(path)
	require.NoError(t, err)
	require.NotNil(t, pool)
}

// TestCertPoolFromFile_Missing verifies that a missing file is
// rejected.
func TestCertPoolFromFile_Missing(t *testing.T) {
	t.Parallel()

	_, err := certPoolFromFile("/nonexistent/tls.cert")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unable to read TLS cert")
}
