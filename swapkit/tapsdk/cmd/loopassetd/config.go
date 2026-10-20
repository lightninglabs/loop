package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	sdk "github.com/lightninglabs/tap-sdk"
)

type walletConfig struct {
	Address  string `json:"address"`
	TLSCert  string `json:"tls_cert"`
	Macaroon string `json:"macaroon"`
}

type bitcoinConfig struct {
	URL         string `json:"url"`
	TLSCert     string `json:"tls_cert"`
	Credentials string `json:"credentials"`
}

type config struct {
	Network           sdk.Network   `json:"network"`
	StateDir          string        `json:"state_dir"`
	Socket            string        `json:"socket"`
	Token             string        `json:"token"`
	LND               walletConfig  `json:"lnd"`
	Tapd              walletConfig  `json:"tapd"`
	Bitcoin           bitcoinConfig `json:"bitcoin"`
	FeeSat            uint64        `json:"fee_sat"`
	MaxFeeSat         uint64        `json:"max_fee_sat"`
	MinConfs          uint32        `json:"min_confs"`
	RPCTimeoutSeconds uint32        `json:"rpc_timeout_seconds"`
}

type bitcoinCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func loadConfig(path string) (*config, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("config path must be absolute and clean")
	}
	raw, err := readRegular(path, false)
	if err != nil {
		return nil, errors.New("cannot read runtime configuration")
	}
	var cfg config
	if strictJSON(raw, &cfg) != nil {
		return nil, errors.New("invalid runtime configuration JSON")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *config) validate() error {
	if networkParams(c.Network) == nil {
		return errors.New("an explicit Bitcoin network is required")
	}
	for _, path := range []string{
		c.StateDir, c.Socket, c.Token, c.LND.TLSCert, c.LND.Macaroon,
		c.Tapd.TLSCert, c.Tapd.Macaroon, c.Bitcoin.Credentials,
	} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("runtime paths must be absolute")
		}
	}
	if filepath.Dir(c.Socket) != c.StateDir {
		return errors.New("socket must be inside its state directory")
	}
	for _, address := range []string{c.LND.Address, c.Tapd.Address} {
		host, port, err := net.SplitHostPort(address)
		number, numberErr := strconv.ParseUint(port, 10, 16)
		if err != nil || numberErr != nil || number == 0 || host == "" {
			return errors.New("wallet RPC requires host:port")
		}
	}
	endpoint, err := url.Parse(c.Bitcoin.URL)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" {

		return errors.New("invalid Bitcoin RPC endpoint")
	}
	switch endpoint.Scheme {
	case "https":
	case "http":
		// Cleartext RPC is permitted only for an explicit loopback IP;
		// DNS names could resolve outside the local trust boundary.
		ip := net.ParseIP(endpoint.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return errors.New("remote Bitcoin RPC requires TLS")
		}
	default:
		return errors.New("Bitcoin RPC requires an HTTP or HTTPS URL")
	}
	if c.Bitcoin.TLSCert != "" &&
		(!filepath.IsAbs(c.Bitcoin.TLSCert) ||
			filepath.Clean(c.Bitcoin.TLSCert) !=
				c.Bitcoin.TLSCert) {

		return errors.New("Bitcoin TLS path must be absolute")
	}
	if c.FeeSat == 0 || c.MaxFeeSat < c.FeeSat || c.MinConfs == 0 ||
		c.RPCTimeoutSeconds == 0 || c.RPCTimeoutSeconds > 60 {

		return errors.New("fee, confirmation or timeout policy missing")
	}
	return nil
}

func networkParams(network sdk.Network) *chaincfg.Params {
	switch network {
	case sdk.NetworkMainnet:
		return &chaincfg.MainNetParams
	case sdk.NetworkTestnet:
		return &chaincfg.TestNet3Params
	case sdk.NetworkTestnet4:
		return &chaincfg.TestNet4Params
	case sdk.NetworkRegtest:
		return &chaincfg.RegressionNetParams
	case sdk.NetworkSimnet:
		return &chaincfg.SimNetParams
	case sdk.NetworkSignet:
		return &chaincfg.SigNetParams
	default:
		return nil
	}
}

func (c *config) timeout() time.Duration {
	return time.Duration(c.RPCTimeoutSeconds) * time.Second
}

func strictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

// readRegular refuses links and broad permissions for secret material before
// reading it. Error messages intentionally never include file contents.
func readRegular(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("invalid runtime input file")
	}
	mode := info.Mode().Perm()
	if private && mode != 0400 && mode != 0600 {
		return nil, errors.New("credentials require mode 0400 or 0600")
	}
	return os.ReadFile(path)
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return errors.New("cannot create runtime state directory")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("runtime state directory requires mode 0700")
	}
	return nil
}
