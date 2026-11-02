package rest

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// TLSSource describes how the REST client should build its TLS
// trust configuration. Exactly one source is held on Config.TLS, so
// mutually-exclusive trust choices (pinned cert vs system pool vs
// insecure) cannot be expressed. Instances are obtained from
// TLSFromPath, TLSFromData, TLSSystemCert, or TLSInsecure.
type TLSSource interface {
	// tlsConfig builds the *tls.Config used by the underlying
	// http.Transport.
	tlsConfig(minVersion uint16) (*tls.Config, error)
}

// TLSFromPath returns a TLSSource that loads a PEM-encoded
// certificate from disk and trusts exactly that cert.
func TLSFromPath(path string) TLSSource {
	return tlsPathSource{path: path}
}

// TLSFromData returns a TLSSource that trusts a PEM certificate
// supplied directly as a string.
func TLSFromData(pem string) TLSSource {
	return tlsDataSource{pem: pem}
}

// TLSSystemCert returns a TLSSource that uses the host's system
// certificate pool.
func TLSSystemCert() TLSSource {
	return tlsSystemSource{}
}

// TLSInsecure returns a TLSSource that disables TLS verification.
// Intended for local development only.
func TLSInsecure() TLSSource {
	return tlsInsecureSource{}
}

type tlsPathSource struct {
	path string
}

func (s tlsPathSource) tlsConfig(minVersion uint16) (*tls.Config, error) {
	return tlsConfigFromFile(s.path, minVersion)
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

// tlsConfigFromFile reads a PEM certificate file and returns a TLS
// config that trusts it.
func tlsConfigFromFile(path string,
	minVersion uint16) (*tls.Config, error) {

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("unable to read TLS cert at %s: %v",
			path, err)
	}

	pool, err := certPoolFromPEM(data)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		RootCAs:    pool,
		MinVersion: minVersion,
	}, nil
}
