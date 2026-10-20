package loopd

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/assets/assetln"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetclient"
	"github.com/lightninglabs/loop/swapkit"
	assetkit "github.com/lightninglabs/loop/swapkit/tapsdk"
	sdk "github.com/lightninglabs/tap-sdk"
	sdkgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/tap-sdk/macaroon"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"google.golang.org/grpc"
)

// assetLoopInConfig rejects partial activation before any funding is possible.
func assetLoopInConfig(cfg *Config) ([33]byte, bool, error) {
	var peer [33]byte
	enabled := cfg.AssetPeer != "" || cfg.AssetBitcoinURL != ""
	if !enabled {
		return peer, false, nil
	}
	backend, err := url.Parse(cfg.AssetBitcoinURL)
	if cfg.Tapd == nil || !cfg.Tapd.Activate || err != nil ||
		backend.Host == "" || backend.User != nil ||
		(backend.Scheme != "http" && backend.Scheme != "https") ||
		cfg.AssetFeeSat == 0 || cfg.AssetMaxFeeSat < cfg.AssetFeeSat {

		return peer, false, fmt.Errorf("asset loop-in requires tapd, " +
			"Bitcoin RPC and valid anchor fee limits")
	}
	raw, err := hex.DecodeString(cfg.AssetPeer)
	if err != nil || len(raw) != len(peer) ||
		hex.EncodeToString(raw) != cfg.AssetPeer {

		return peer, false, fmt.Errorf("invalid asset peer")
	}
	key, err := btcec.ParsePubKey(raw)
	if err != nil || hex.EncodeToString(key.SerializeCompressed()) !=
		cfg.AssetPeer {

		return peer, false, fmt.Errorf("invalid asset peer")
	}
	copy(peer[:], raw)
	return peer, true, nil
}

// assetAuthConn adds the existing LND macaroon to raw generated RPC clients.
type assetAuthConn struct{ services *lndclient.LndServices }

func (c assetAuthConn) Invoke(ctx context.Context, method string,
	args, reply any, opts ...grpc.CallOption) error {

	ctx, err := c.services.WithMacaroonAuthForService(
		ctx, lndclient.AdminServiceMac,
	)
	if err != nil {
		return err
	}
	return c.services.ClientConn.Invoke(ctx, method, args, reply, opts...)
}

func (c assetAuthConn) NewStream(ctx context.Context, desc *grpc.StreamDesc,
	method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {

	ctx, err := c.services.WithMacaroonAuthForService(
		ctx, lndclient.AdminServiceMac,
	)
	if err != nil {
		return nil, err
	}
	return c.services.ClientConn.NewStream(ctx, desc, method, opts...)
}

// newAssetConfig connects the kit to normal wallet and SQL services.
func (d *Daemon) newAssetConfig(client *loop.Client,
	addresses *address.Manager, deposits *deposit.Manager,
	store *loopin.SqlStore) (*loopin.AssetConfig, error) {

	peer, enabled, err := assetLoopInConfig(d.cfg)
	if err != nil || !enabled {
		return nil, err
	}
	if d.assetClient == nil {
		return nil, fmt.Errorf(
			"asset loop-in requires an initialized " +
				"tapd connection",
		)
	}
	sdkClient, err := sdkgrpc.NewClient(&sdkgrpc.Config{
		Host: d.cfg.Tapd.Host, Network: sdk.Network(d.cfg.Network),
		TLS:      sdkgrpc.TLSFromPath(d.cfg.Tapd.TLSPath),
		Macaroon: macaroon.FromPath(d.cfg.Tapd.MacaroonPath),
	})
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{}
	if d.cfg.AssetBitcoinTLS != "" {
		raw, err := os.ReadFile(d.cfg.AssetBitcoinTLS)
		if err != nil {
			sdkClient.Close()
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(raw) {
			sdkClient.Close()
			return nil, fmt.Errorf("invalid Bitcoin TLS certificate")
		}
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    roots,
				MinVersion: tls.VersionTLS12,
			},
		}
	}
	utxos := &assetkit.BitcoinUTXO{
		URL:      d.cfg.AssetBitcoinURL,
		User:     d.cfg.AssetBitcoinUser,
		Password: d.cfg.AssetBitcoinPassword,
		Client:   httpClient,
	}
	chain := &assetkit.LNDChain{
		Kit:           d.lnd.ChainKit,
		Notifier:      d.lnd.ChainNotifier,
		UnspentOutput: utxos.Unspent,
	}
	network := sdk.Network(d.cfg.Network)
	driver, err := assetkit.NewDriver(assetkit.DriverConfig{
		Network:    network,
		SDK:        sdkClient,
		Wallet:     sdk.NewWallet(sdkClient, network),
		WalletKit:  d.lnd.WalletKit,
		Signer:     d.lnd.Signer,
		Chain:      chain,
		NodePubkey: d.lnd.NodePubkey,
		FeeSat:     d.cfg.AssetFeeSat,
		MaxFeeSat:  d.cfg.AssetMaxFeeSat,
		MinConfs:   1,
		Deposit: func(ctx context.Context, point string) (
			swapkit.Deposit, keychain.KeyLocator, error) {

			params, err := addresses.GetStaticAddressParameters(ctx)
			if err != nil {
				return swapkit.Deposit{}, keychain.KeyLocator{}, err
			}
			entries, err := store.AssetInventory(ctx)
			if err != nil {
				return swapkit.Deposit{}, keychain.KeyLocator{}, err
			}
			for _, entry := range entries {
				if entry.Deposit.Outpoint == point {
					return entry.Deposit, params.KeyLocator, nil
				}
			}
			return swapkit.Deposit{}, keychain.KeyLocator{},
				swapkit.ErrInvalid
		},
	})
	if err != nil {
		sdkClient.Close()
		httpClient.CloseIdleConnections()
		return nil, err
	}
	auth := assetAuthConn{services: &d.lnd.LndServices}
	var node [33]byte
	copy(node[:], d.lnd.NodePubkey[:])
	cfg := &loopin.AssetConfig{
		Driver: driver, Store: store,
		Address: addresses, Deposits: deposits,
		Server: assetclient.NewServerClient(client.Conn),
		Receiver: &assetln.Receiver{
			Assets:    d.assetClient.TaprootAssetChannelsClient,
			Lightning: lnrpc.NewLightningClient(auth),
			Invoices:  invoicesrpc.NewInvoicesClient(auth),
		},
		Owner: sha256.Sum256(node[:]), Peer: peer,
		Network: d.cfg.Network, NodePubkey: node,
		FetchL402: client.Server.FetchL402,
		OnError: func(err error) {
			warnf("Asset recovery operation pending: %v", err)
		},
	}
	d.assetCleanup = func() {
		sdkClient.Close()
		httpClient.CloseIdleConnections()
	}
	return cfg, nil
}
