package grpc

import (
	"crypto/tls"
)

// TLSSource describes how the gRPC client should build its TLS trust
// configuration. Exactly one source is held on Config.TLS — so
// mutually exclusive choices (pinned cert vs system pool vs
// insecure) cannot be expressed. Instances are obtained from
// TLSFromPath, TLSFromData, TLSSystemCert, or TLSInsecure.
type TLSSource interface {
	// tlsConfig builds the base *tls.Config for the chosen trust
	// mode. The gRPC client layer applies pinning and transport
	// credential wrapping on top.
	tlsConfig(minVersion uint16) (*tls.Config, error)
}

// TLSFromPath returns a TLSSource that loads a PEM-encoded
// certificate from disk and trusts exactly that cert.
func TLSFromPath(path string) TLSSource {
	return tlsPathSource{path: path}
}

// TLSFromData returns a TLSSource that trusts a PEM certificate
// supplied directly as a string (useful when the cert travels
// through config management rather than the filesystem).
func TLSFromData(pem string) TLSSource {
	return tlsDataSource{pem: pem}
}

// TLSSystemCert returns a TLSSource that uses the host's system
// certificate pool, appropriate for reaching tapd through a
// publicly-issued certificate chain.
func TLSSystemCert() TLSSource {
	return tlsSystemSource{}
}

// TLSInsecure returns a TLSSource that disables TLS verification.
// Intended for local bufconn / regtest setups only — never for
// production.
func TLSInsecure() TLSSource {
	return tlsInsecureSource{}
}

type tlsPathSource struct {
	path string
}

func (s tlsPathSource) tlsConfig(minVersion uint16) (*tls.Config, error) {
	pool, err := certPoolFromFile(s.path)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		RootCAs:    pool,
		MinVersion: minVersion,
	}, nil
}

type tlsDataSource struct {
	pem string
}

func (s tlsDataSource) tlsConfig(minVersion uint16) (*tls.Config, error) {
	pool, err := certPoolFromPEM([]byte(s.pem))
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		RootCAs:    pool,
		MinVersion: minVersion,
	}, nil
}

type tlsSystemSource struct{}

func (tlsSystemSource) tlsConfig(minVersion uint16) (*tls.Config, error) {
	// An empty tls.Config uses the system pool — this matches
	// x509.SystemCertPool() but avoids the Windows-specific
	// limitation of that helper.
	return &tls.Config{
		MinVersion: minVersion,
	}, nil
}

type tlsInsecureSource struct{}

func (tlsInsecureSource) tlsConfig(minVersion uint16) (*tls.Config, error) {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
		MinVersion:         minVersion,
	}, nil
}
