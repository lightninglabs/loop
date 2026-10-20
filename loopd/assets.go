package loopd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/assets/assetln"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetmanager"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"google.golang.org/grpc"
)

// assetRuntimeConfig rejects partial activation before any funding is possible.
func assetRuntimeConfig(cfg *Config) ([33]byte, bool, error) {
	var peer [33]byte
	enabled := cfg.AssetRuntimeSocket != "" ||
		cfg.AssetRuntimeTokenFile != "" || cfg.AssetPeer != ""
	if !enabled {
		return peer, false, nil
	}
	if cfg.Tapd == nil || !cfg.Tapd.Activate ||
		!filepath.IsAbs(cfg.AssetRuntimeSocket) ||
		!filepath.IsAbs(cfg.AssetRuntimeTokenFile) {

		return peer, false, fmt.Errorf("asset loop-in requires tapd, " +
			"an absolute runtime socket and token path")
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

// newAssetManager restores inventory exclusion before Bitcoin deposit recovery.
func (d *Daemon) newAssetManager(ctx context.Context, client *loop.Client,
	addresses *address.Manager, deposits *deposit.Manager) (
	*assetmanager.Manager, error) {

	peer, enabled, err := assetRuntimeConfig(d.cfg)
	if err != nil || !enabled {
		return nil, err
	}
	if d.assetClient == nil {
		return nil, fmt.Errorf(
			"asset loop-in requires an initialized " +
				"tapd connection",
		)
	}
	info, err := os.Lstat(d.cfg.AssetRuntimeTokenFile)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf(
			"asset runtime token must be a private file",
		)
	}
	token, err := os.ReadFile(d.cfg.AssetRuntimeTokenFile)
	if err != nil {
		return nil, err
	}
	runtime, err := bridge.NewClient(d.cfg.AssetRuntimeSocket, token)
	if err != nil {
		return nil, err
	}
	inventory, err := assetmanager.OpenStore(filepath.Join(
		d.cfg.DataDir, "asset-inventory.db",
	))
	if err != nil {
		return nil, err
	}
	journal, err := assetexec.OpenBoltStore(filepath.Join(
		d.cfg.DataDir, "asset-loop-in.db",
	))
	if err != nil {
		_ = inventory.Close()
		return nil, err
	}
	cleanup := func() {
		runtime.Close()
		if err := journal.Close(); err != nil {
			errorf("Closing asset swap journal: %v", err)
		}
		if err := inventory.Close(); err != nil {
			errorf("Closing asset inventory: %v", err)
		}
	}
	auth := assetAuthConn{services: &d.lnd.LndServices}
	var node [33]byte
	copy(node[:], d.lnd.NodePubkey[:])
	manager, err := assetmanager.New(assetmanager.Config{
		Runtime: runtime, Store: inventory, Journal: journal,
		Address: addresses, Deposits: deposits,
		Server: swapserverrpc.NewAssetLoopInServerClient(client.Conn),
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
	})
	if err == nil {
		err = manager.Initialize(ctx)
	}
	if err != nil {
		cleanup()
		return nil, err
	}
	d.assetCleanup = cleanup
	return manager, nil
}
