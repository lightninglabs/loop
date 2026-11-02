package rest

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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func generateSelfSignedCert(t *testing.T) ([]byte, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"tap-sdk-rest-test"},
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

func TestBuildTLSConfigMinVersion(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		TLS:           TLSSystemCert(),
		TLSMinVersion: tls.VersionTLS13,
	}

	tlsCfg, err := buildTLSConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, uint16(tls.VersionTLS13), tlsCfg.MinVersion)
}

func TestBuildTLSConfigPinnedFingerprint(t *testing.T) {
	t.Parallel()

	certPEM, derBytes := generateSelfSignedCert(t)
	digest := sha256.Sum256(derBytes)

	cfg := &Config{
		TLS: TLSFromData(string(certPEM)),
		TLSPinnedCertFingerprint: hex.EncodeToString(
			digest[:],
		),
	}

	tlsCfg, err := buildTLSConfig(cfg)
	require.NoError(t, err)
	require.NotNil(t, tlsCfg.VerifyConnection)
	cert, err := x509.ParseCertificate(derBytes)
	require.NoError(t, err)
	for _, resumed := range []bool{false, true} {
		state := tls.ConnectionState{
			DidResume:        resumed,
			PeerCertificates: []*x509.Certificate{cert},
		}
		require.NoError(t, tlsCfg.VerifyConnection(state))
		state.PeerCertificates = []*x509.Certificate{{Raw: []byte("wrong")}}
		require.ErrorContains(
			t, tlsCfg.VerifyConnection(state), "fingerprint mismatch",
		)
		state.PeerCertificates = nil
		require.ErrorContains(
			t, tlsCfg.VerifyConnection(state), "no certificates",
		)
	}
}

func TestBuildTLSConfigInvalidPinnedFingerprint(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		TLS:                      TLSSystemCert(),
		TLSPinnedCertFingerprint: "not-hex",
	}

	_, err := buildTLSConfig(cfg)
	require.ErrorContains(t, err, "TLSPinnedCertFingerprint")
}

func TestValidateBaseURL(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateBaseURL("https://localhost:8089"))
	require.ErrorContains(
		t, validateBaseURL("http://localhost:8089"),
		"unsupported base URL scheme",
	)
	require.ErrorContains(
		t, validateBaseURL("https:///missing-host"),
		"base URL must include a host",
	)
}
