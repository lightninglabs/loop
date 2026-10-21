package loopd

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	assetkit "github.com/lightninglabs/loop/swapkit/tapsdk"
	sdk "github.com/lightninglabs/tap-sdk"
	sdkgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"github.com/lightninglabs/tap-sdk/macaroon"
	"github.com/lightninglabs/taproot-assets/taprpc"
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
		backend.RawQuery != "" || backend.Fragment != "" ||
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

// newAssetConfig connects the kit to the normal node services.
func (d *Daemon) newAssetConfig(client *loop.Client,
	addresses *address.Manager, deposits *deposit.Manager) (
	*loopin.AssetConfig, error) {

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
	bitcoin, err := assetBitcoinRPC(d.cfg)
	if err != nil {
		sdkClient.Close()
		return nil, err
	}
	chain := &assetkit.LNDChain{
		Kit:      d.lnd.ChainKit,
		Notifier: d.lnd.ChainNotifier,
		UnspentOutput: func(ctx context.Context,
			point wire.OutPoint) (bool, error) {

			return assetUnspent(ctx, bitcoin, point)
		},
	}
	var node [33]byte
	copy(node[:], d.lnd.NodePubkey[:])
	cfg := &loopin.AssetConfig{
		AssetConfig: deposit.AssetConfig{
			Wallet: sdk.NewWallet(sdkClient, sdk.Network(d.cfg.Network)),
			Chain:  chain, FeeSat: d.cfg.AssetFeeSat,
			MaxFeeSat: d.cfg.AssetMaxFeeSat, MinConfs: 1,
		},
		Address: addresses, Deposits: deposits,
		Server: loopin.NewAssetServerClient(client.Conn),
		Tapd:   d.assetClient.TaprootAssetChannelsClient,

		Owner: sha256.Sum256(node[:]), Peer: peer,
		FetchL402: client.Server.FetchL402,
	}
	d.assetCleanup = func() {
		sdkClient.Close()
		bitcoin.Shutdown()
	}
	return cfg, nil
}

// assetBitcoinRPC uses the standard Bitcoin client for external anchor UTXOs.
// LND wallet transactions cannot prove that these outputs remain unspent.
func assetBitcoinRPC(cfg *Config) (*rpcclient.Client, error) {
	backend, err := url.Parse(cfg.AssetBitcoinURL)
	if err != nil {
		return nil, err
	}
	var certificate []byte
	if cfg.AssetBitcoinTLS != "" {
		certificate, err = os.ReadFile(cfg.AssetBitcoinTLS)
		if err != nil {
			return nil, err
		}
		if !x509.NewCertPool().AppendCertsFromPEM(certificate) {
			return nil, fmt.Errorf("invalid Bitcoin TLS certificate")
		}
	}
	return rpcclient.New(&rpcclient.ConnConfig{
		Host: backend.Host + backend.EscapedPath(),
		User: cfg.AssetBitcoinUser, Pass: cfg.AssetBitcoinPassword,
		HTTPPostMode: true, DisableTLS: backend.Scheme == "http",
		Certificates: certificate,
	}, nil)
}

// assetUnspent checks external anchors against the UTXO set and mempool.
// Cancellation stops this operation from proceeding to a signing request.
func assetUnspent(ctx context.Context, client *rpcclient.Client,
	point wire.OutPoint) (bool, error) {

	if err := ctx.Err(); err != nil {
		return false, err
	}
	future := client.GetTxOutAsync(&point.Hash, point.Index, true)
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case response := <-future:
		ready := make(rpcclient.FutureGetTxOutResult, 1)
		ready <- response
		out, err := ready.Receive()
		return out != nil, err
	}
}

// validateAssetLoopInNode adds the chain checks needed before restoring asset
// deposits to the ordinary daemon's tapd identity check.
func validateAssetLoopInNode(info *taprpc.GetInfoResponse, network string,
	height uint32) error {

	if info.Network != network {
		return fmt.Errorf("asset node network mismatch")
	}
	if !info.SyncToChain || info.BlockHeight != height {
		return fmt.Errorf("asset node is not synchronized")
	}
	return nil
}
