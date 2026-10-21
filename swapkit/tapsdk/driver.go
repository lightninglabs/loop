package tapsdk

import (
	"context"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/keychain"
)

// DriverConfig supplies the normal daemon's node services. Deposit reads the
// inventory and static address key locator; only the client uses it.
type DriverConfig struct {
	Network    sdk.Network
	SDK        sdk.Client
	Wallet     *sdk.Wallet
	WalletKit  lndclient.WalletKitClient
	Signer     lndclient.SignerClient
	Chain      Chain
	NodePubkey [33]byte
	Deposit    func(context.Context, string) (
		swapkit.Deposit, keychain.KeyLocator, error)
	FeeSat    uint64
	MaxFeeSat uint64
	MinConfs  uint32
}

// Driver contains dependencies only. Deposit and swap FSM actions own signed
// transaction persistence and recovery.
type Driver struct{ cfg DriverConfig }

// NewDriver configures transaction operations without a database or worker.
func NewDriver(cfg DriverConfig) (*Driver, error) {
	if cfg.SDK == nil || cfg.Wallet == nil || cfg.WalletKit == nil ||
		cfg.Signer == nil || cfg.Chain == nil || cfg.FeeSat == 0 ||
		cfg.MaxFeeSat < cfg.FeeSat || cfg.MinConfs == 0 {

		return nil, swapkit.ErrInvalid
	}
	return &Driver{cfg: cfg}, nil
}

// CheckReady verifies the tapd connection belongs to the normal signing node.
func (r *Driver) CheckReady(ctx context.Context) error {
	info, err := r.cfg.SDK.GetInfo(ctx)
	if err != nil {
		return err
	}
	if info == nil || info.LndIdentityPubkey != r.cfg.NodePubkey ||
		info.Network != string(r.cfg.Network) {

		return swapkit.ErrInvalid
	}
	if !info.SyncedToChain {
		return swapkit.ErrPending
	}
	tip, err := r.cfg.Chain.Height(ctx)
	if err != nil {
		return err
	}
	if info.BlockHeight != tip {
		return swapkit.ErrPending
	}
	return nil
}

// Height reads the normal daemon's chain service.
func (r *Driver) Height(ctx context.Context) (uint32, error) {
	return r.cfg.Chain.Height(ctx)
}

// ValidateOffer verifies the deposit policy, lineage and current spendability.
func (r *Driver) ValidateOffer(ctx context.Context, offer swapkit.Offer) error {
	_, err := r.offerDeposit(ctx, offer, true)
	return err
}

// VerifyAdmission checks all on-chain inputs against registered static keys.
func (r *Driver) VerifyAdmission(ctx context.Context, a swapkit.Admission) (
	swapkit.AdmissionResult, error) {

	return r.admit(ctx, a)
}

// ValidateInvoice checks the fixed fallback package before payment dispatch.
func (r *Driver) ValidateInvoice(ctx context.Context, offer swapkit.Offer,
	preparation swapkit.Preparation) error {

	s, err := r.preparedSwap(ctx, offer,
		preparation.RecoveryRequest, true)
	if err != nil {
		return err
	}
	// Apply current operator limits only before accepting preparation.
	// Signing retries and recovery validate the fixed package instead.
	if s.Fallback == nil ||
		s.Fallback.Funding.ActualFeeSat > r.cfg.MaxFeeSat {

		return swapkit.ErrInvalid
	}
	return nil
}
