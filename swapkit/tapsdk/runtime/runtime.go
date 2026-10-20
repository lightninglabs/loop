// Package runtime implements Loop's durable SDK transaction and proof driver.
// Wallet signing keys remain in LND. This process journals exact public
// transaction packages; walletsigner separately protects nonce recovery.
package runtime

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightninglabs/loop/swapkit/tapsdk/walletsigner"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	bolt "go.etcd.io/bbolt"
)

// Config binds one runtime to one authenticated wallet and Bitcoin network.
type Config struct {
	Path      string
	Network   sdk.Network
	SDK       sdk.Client
	Wallet    *sdk.Wallet
	Lightning lnrpc.LightningClient
	WalletKit walletrpc.WalletKitClient
	Signer    *walletsigner.Signer
	Chain     Chain
	FeeSat    uint64
	MaxFeeSat uint64
	MinConfs  uint32
}

// Runtime owns a durable transaction journal and serialized wallet effects.
type Runtime struct {
	cfg  Config
	db   *bolt.DB
	gate chan struct{}
}

// Open reopens exact deposit, signing-package and publication intents.
func Open(cfg Config) (*Runtime, error) {
	if cfg.Path == "" || cfg.SDK == nil || cfg.Wallet == nil ||
		cfg.Lightning == nil || cfg.WalletKit == nil ||
		cfg.Signer == nil ||
		cfg.Chain == nil || cfg.FeeSat == 0 ||
		cfg.MaxFeeSat < cfg.FeeSat || cfg.MinConfs == 0 {
		return nil, bridge.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(cfg.Path)
	if err == nil && (!info.Mode().IsRegular() ||
		info.Mode().Perm() != 0600) {

		return nil, bridge.ErrInvalid
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	db, err := bolt.Open(cfg.Path, 0600,
		&bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("runtime-v1"))
		return err
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Runtime{cfg: cfg, db: db, gate: make(chan struct{}, 1)}, nil
}

// Close joins no background work; callers stop the bridge before closing it.
func (r *Runtime) Close() error { return r.db.Close() }

func (r *Runtime) get(key string, target any) error {
	return r.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("runtime-v1")).Get([]byte(key))
		if raw == nil {
			return os.ErrNotExist
		}
		return json.Unmarshal(raw, target)
	})
}

func (r *Runtime) put(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return r.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("runtime-v1")).Put([]byte(key), raw)
	})
}

// Call dispatches only protocol-bound operations after local authentication.
func (r *Runtime) Call(ctx context.Context, method bridge.Method,
	payload json.RawMessage) (any, error) {

	select {
	case r.gate <- struct{}{}:
		defer func() { <-r.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	switch method {
	case bridge.CheckReady:
		return r.ready(ctx)
	case bridge.Height:
		return r.cfg.Chain.Height(ctx)
	case bridge.CreateDeposit:
		var request bridge.CreateDepositRequest
		if err := bridge.Decode(payload, &request); err != nil {
			return nil, bridge.ErrInvalid
		}
		return r.createDeposit(ctx, request)
	case bridge.Inventory:
		var request bridge.InventoryRequest
		if err := bridge.Decode(payload, &request); err != nil {
			return nil, bridge.ErrInvalid
		}
		return r.inventory(ctx, request)
	case bridge.RefundDeposit:
		var request bridge.RefundDepositRequest
		if err := bridge.Decode(payload, &request); err != nil {
			return nil, bridge.ErrInvalid
		}
		return r.refundDeposit(ctx, request)
	case bridge.VerifyAdmission:
		var admission bridge.Admission
		if err := bridge.Decode(payload, &admission); err != nil {
			return nil, bridge.ErrInvalid
		}
		return r.admit(ctx, admission)
	}
	var op bridge.Operation
	if err := bridge.Decode(payload, &op); err != nil {
		return nil, bridge.ErrInvalid
	}
	if op.Offer.Hash == ([32]byte{}) {
		return nil, bridge.ErrInvalid
	}
	switch method {
	case bridge.ValidateDeposits, bridge.ValidateOffer:
		_, err := r.offerDeposit(ctx, op.Offer, true)
		return struct{}{}, err
	case bridge.PrepareRecovery:
		return r.prepareRecovery(ctx, op.Offer)
	case bridge.ValidateInvoice:
		_, err := r.validateRecovery(ctx, op.Offer,
			op.Preparation.RecoveryRequest)
		return struct{}{}, err
	case bridge.BuildFallback:
		return r.buildFallback(ctx, op)
	case bridge.VerifyFinalizeFallback:
		return r.finalizeFallback(ctx, op)
	case bridge.VerifyClaim:
		return r.verifyClaim(ctx, op)
	case bridge.ValidateExecution:
		return struct{}{}, r.validateExecution(ctx, op)
	case bridge.RevealPreimage:
		// Authenticated server Get calls recover the disclosure.
		// Independent claiming does not require a notification.
		return struct{}{}, nil
	case bridge.RecoverClaim:
		return r.recoverClaim(ctx, op)
	case bridge.RecoverRefund:
		return r.recoverRefund(ctx, op)
	case bridge.InspectRefund, bridge.StoreRefundProof:
		return r.inspectRefund(ctx, op)
	case bridge.InspectClaim:
		return r.inspectClaim(ctx, op)
	case bridge.LookupPreimage:
		return r.lookupPreimage(ctx, op)
	case bridge.SignCooperative:
		// Independent HTLC recovery is always available. A direct sweep
		// must never be signed without a separately validated proposal.
		return nil, bridge.ErrPending
	default:
		return nil, bridge.ErrInvalid
	}
}

func (r *Runtime) ready(ctx context.Context) (bridge.Ready, error) {
	info, err := r.cfg.SDK.GetInfo(ctx)
	if err != nil {
		return bridge.Ready{}, err
	}
	lnd, err := r.cfg.Lightning.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return bridge.Ready{}, err
	}
	if info == nil || lnd == nil {
		return bridge.Ready{}, bridge.ErrInvalid
	}
	key, err := hex.DecodeString(lnd.IdentityPubkey)
	if err != nil || hex.EncodeToString(key) != lnd.IdentityPubkey ||
		!bytes.Equal(key, info.LndIdentityPubkey[:]) ||
		info.Network != string(r.cfg.Network) {
		return bridge.Ready{}, bridge.ErrInvalid
	}
	if !info.SyncedToChain || !lnd.SyncedToChain {
		return bridge.Ready{}, bridge.ErrPending
	}
	tip, err := r.cfg.Chain.Height(ctx)
	if err != nil {
		return bridge.Ready{}, err
	}
	if info.BlockHeight != tip || lnd.BlockHeight != tip {
		return bridge.Ready{}, bridge.ErrPending
	}
	identity := bridge.Ready{
		Network: info.Network, LndPubkey: info.LndIdentityPubkey,
	}
	var stored bridge.Ready
	err = r.get("identity/v1", &stored)
	if err == nil && !equal(stored, identity) {
		return bridge.Ready{}, bridge.ErrInvalid
	}
	if err != nil && !missing(err) {
		return bridge.Ready{}, err
	}
	if missing(err) {
		if err := r.put("identity/v1", identity); err != nil {
			return bridge.Ready{}, err
		}
	}
	return identity, nil
}

func equal(a, b any) bool {
	x, xerr := json.Marshal(a)
	y, yerr := json.Marshal(b)
	return xerr == nil && yerr == nil && bytes.Equal(x, y)
}

func key(prefix string, id [32]byte) string {
	return prefix + hex.EncodeToString(id[:])
}

func missing(err error) bool { return errors.Is(err, os.ErrNotExist) }
