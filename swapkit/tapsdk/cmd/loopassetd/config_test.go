package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

func validConfig(t *testing.T) config {
	t.Helper()
	dir := t.TempDir()
	return config{
		Network: sdk.NetworkRegtest, StateDir: dir,
		Socket: filepath.Join(dir, "runtime.sock"),
		Token:  filepath.Join(dir, "bridge.token"),
		LND: walletConfig{
			Address:  "127.0.0.1:10009",
			TLSCert:  filepath.Join(dir, "lnd.cert"),
			Macaroon: filepath.Join(dir, "lnd.macaroon"),
		},
		Tapd: walletConfig{
			Address:  "127.0.0.1:10029",
			TLSCert:  filepath.Join(dir, "tapd.cert"),
			Macaroon: filepath.Join(dir, "tapd.macaroon"),
		},
		Bitcoin: bitcoinConfig{
			URL:         "http://127.0.0.1:18443/",
			Credentials: filepath.Join(dir, "bitcoin.json"),
		},
		FeeSat: 1000, MaxFeeSat: 2000, MinConfs: 3,
		RPCTimeoutSeconds: 10,
	}
}

func TestExplicitConfigAndCredentialBoundaries(t *testing.T) {
	cases := map[string]func(*config){
		"network required": func(c *config) { c.Network = "" },
		"relative secret": func(c *config) {
			c.Token = "bridge.token"
		},
		"remote cleartext": func(c *config) {
			c.Bitcoin.URL = "http://192.0.2.1:18443"
		},
		"DNS cleartext": func(c *config) {
			c.Bitcoin.URL = "http://localhost:18443"
		},
		"URL credentials": func(c *config) {
			c.Bitcoin.URL = "https://secret:password@example.com/"
		},
		"URL query": func(c *config) {
			c.Bitcoin.URL = "https://example.com/?secret=password"
		},
		"missing policy":       func(c *config) { c.FeeSat = 0 },
		"unsafe confirmations": func(c *config) { c.MinConfs = 0 },
		"unbounded RPC": func(c *config) {
			c.RPCTimeoutSeconds = 0
		},
		"excessive RPC": func(c *config) {
			c.RPCTimeoutSeconds = 61
		},
		"bad wallet address": func(c *config) {
			c.LND.Address = "lnd"
		},
		"socket elsewhere": func(c *config) {
			c.Socket = "/tmp/other/runtime.sock"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t)
			change(&cfg)
			err := cfg.validate()
			require.Error(t, err)
			require.NotContains(t, err.Error(), "password")
		})
	}
	cfg := validConfig(t)
	require.NoError(t, cfg.validate())
	cfg.Bitcoin.URL = "https://bitcoin.example.com/"
	require.NoError(t, cfg.validate())
}

func TestStrictConfigDoesNotExposeUnknownSecrets(t *testing.T) {
	cfg := validConfig(t)
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	path := filepath.Join(cfg.StateDir, "config.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	decoded, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, cfg, *decoded)
	bad := []byte(`{"password-do-not-print":"secret"}`)
	require.NoError(t, os.WriteFile(path, bad, 0600))
	_, err = loadConfig(path)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "password-do-not-print")
	require.NotContains(t, err.Error(), "secret")
	trailing := append(raw, []byte("{}")...)
	require.NoError(t, os.WriteFile(path, trailing, 0600))
	_, err = loadConfig(path)
	require.Error(t, err)
}

func TestPrivateCredentialFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential")
	require.NoError(t, os.WriteFile(path, []byte("do-not-print"), 0600))
	for _, mode := range []os.FileMode{0600, 0400} {
		require.NoError(t, os.Chmod(path, mode))
		raw, err := readRegular(path, true)
		require.NoError(t, err)
		require.Equal(t, []byte("do-not-print"), raw)
	}
	for _, mode := range []os.FileMode{0644, 0640, 0700} {
		require.NoError(t, os.Chmod(path, mode))
		_, err := readRegular(path, true)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "do-not-print")
	}
	require.NoError(t, os.Chmod(path, 0600))
	link := filepath.Join(dir, "linked")
	require.NoError(t, os.Symlink(path, link))
	_, err := readRegular(link, true)
	require.Error(t, err)
	require.NoError(t, os.Chmod(dir, 0700))
	require.NoError(t, privateDirectory(dir))
	require.NoError(t, os.Chmod(dir, 0755))
	require.Error(t, privateDirectory(dir))
}
