package tapsdk

import (
	"bytes"
	"context"
	"math"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/keychain"
)

// DepositContract binds a public static deposit to its OP_TRUE asset output.
//
//nolint:tagliatelle // Preserve existing serialized contract field names.
type DepositContract struct {
	Version uint32 `json:"Version"`
	// #nosec G117 -- Both keys are public Bitcoin keys.
	ClientKey [33]byte                           `json:"ClientKey"`
	ServerKey [33]byte                           `json:"ServerKey"`
	CSV       uint32                             `json:"CSV"`
	Output    sdk.CustomAnchorAssetOutputSummary `json:"Output"`
}

// DepositContext is operation-local policy and proof material. It owns no
// services, lifecycle or persistence; the ordinary deposit/FSM owns the record.
type DepositContext struct {
	Deposit  swapkit.Deposit
	Contract DepositContract
	Proof    *proof.Proof
	Key      keychain.KeyLocator
}

// SwapContext is reconstructed from one ordinary Loop In record for an action.
// Generic proof validation belongs to the SDK; Loop checks negotiated policy.
type SwapContext struct {
	Offer    swapkit.Offer
	Deposit  *DepositContext
	Fallback *sdk.CustomAnchorTransferPackage
}

// LoadDeposit decodes the normal deposit record and its existing wallet key.
func LoadDeposit(d swapkit.Deposit, key keychain.KeyLocator) (
	*DepositContext, error) {

	var c DepositContract
	if err := swapkit.Decode(d.Contract, &c); err != nil {
		return nil, err
	}
	return &DepositContext{Deposit: d, Contract: c, Key: key}, nil
}

// CheckKey checks that contract authority belongs to the daemon's normal key.
func CheckKey(ctx context.Context, walletKit lndclient.WalletKitClient,
	loc keychain.KeyLocator, expected [33]byte) error {

	if loc.Index > math.MaxInt32 || loc.Family > math.MaxInt32 {
		return swapkit.ErrInvalid
	}
	key, err := walletKit.DeriveKey(ctx, &loc)
	if err != nil {
		return err
	}
	if key == nil || key.PubKey == nil || key.KeyLocator != loc ||
		!bytes.Equal(key.PubKey.SerializeCompressed(), expected[:]) {

		return swapkit.ErrInvalid
	}
	return nil
}

// DepositFromOffer binds the admitted deposit and registration to the server
// offer. The caller verifies its wallet key and on-chain proof before acting.
func DepositFromOffer(offer swapkit.Offer) (*DepositContext, error) {
	t := offer.Terms
	var a swapkit.Admission
	if swapkit.Decode(t.Admission, &a) != nil ||
		a.Registration.Owner != t.Owner || a.RequestID != t.RequestID ||
		a.GroupKey != t.GroupKey || !bytes.Equal(a.Contract, t.Contract) ||
		len(a.Deposits) != 1 || len(t.DepositOutpoints) != 1 ||
		a.Deposits[0].Outpoint != t.DepositOutpoints[0] ||
		a.RefundHeight != t.RefundHeight {

		return nil, swapkit.ErrInvalid
	}
	d := a.Deposits[0]
	d.Contract = t.Contract
	d.Amount, d.GroupKey = t.DepositAmount, t.GroupKey
	record, err := LoadDeposit(d, keychain.KeyLocator{
		Family: keychain.KeyFamily(a.Registration.KeyFamily),
		Index:  a.Registration.KeyIndex})
	if err != nil {
		return nil, err
	}
	c := record.Contract
	if c.ClientKey != a.Registration.ClientKey ||
		c.ServerKey != a.Registration.ServerKey || c.CSV != a.Registration.CSV {

		return nil, swapkit.ErrInvalid
	}
	return record, nil
}

// InspectDeposit checks Loop's contract and current chain before SDK Build
// verifies the input lineage. It does not authorize a spend by itself.
func InspectDeposit(ctx context.Context, chain Chain, minConfs uint32,
	d *DepositContext, unspent bool) error {

	p, err := LastProof(d.Deposit.Proof)
	if err != nil {
		return err
	}
	if err := checkAsset(p, d.Deposit.GroupKey, d.Deposit.Amount); err != nil {
		return err
	}
	if err := checkDepositContract(d.Deposit, d.Contract, p); err != nil {
		return err
	}
	if err := checkConfirmation(ctx, chain, minConfs, p, unspent); err != nil {
		return err
	}
	d.Proof = p
	return nil
}

// VerifyDeposit verifies a deposit outside a builder operation, retaining
// current-chain checks on both sides of the SDK proof verification RPC.
func VerifyDeposit(ctx context.Context, wallet *sdk.Wallet, chain Chain,
	minConfs uint32, d *DepositContext, unspent bool) error {

	if err := InspectDeposit(ctx, chain, minConfs, d, unspent); err != nil {
		return err
	}
	verified, err := wallet.Client().VerifyProof(ctx, d.Deposit.Proof)
	if err != nil {
		return err
	}
	if verified == nil || !verified.Valid {
		return swapkit.ErrInvalid
	}
	return checkConfirmation(ctx, chain, minConfs, d.Proof, unspent)
}

// NewSwap binds the offered policy and SDK fallback to a validated deposit.
func NewSwap(offer swapkit.Offer, d *DepositContext, unsigned []byte) (
	*SwapContext, error) {

	t := offer.Terms
	if d == nil || d.Proof == nil || t.Version != 1 ||
		len(t.DepositOutpoints) != 1 || t.DepositAmount == 0 || t.ChangeAmount != 0 ||
		t.PayoutAmount > t.DepositAmount ||
		t.AssetFee != t.DepositAmount-t.PayoutAmount ||
		d.Deposit.Outpoint != t.DepositOutpoints[0] ||
		!bytes.Equal(d.Deposit.Contract, t.Contract) ||
		d.Deposit.GroupKey != t.GroupKey || d.Deposit.Amount != t.DepositAmount ||
		uint64(d.Proof.BlockHeight)+uint64(d.Contract.CSV) <=
			uint64(t.RefundHeight)+uint64(t.RecoveryDelta) {

		return nil, swapkit.ErrInvalid
	}
	s := &SwapContext{Offer: offer, Deposit: d}
	if len(unsigned) > 0 {
		var err error
		s.Fallback, err = validateRecoveryPackage(offer, d, unsigned)
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}
func checkDepositContract(d swapkit.Deposit, c DepositContract,
	p *proof.Proof) error {

	if c.Version != 1 || c.Output.OPTrueSpend == nil ||
		c.Output.Amount != d.Amount {

		return swapkit.ErrInvalid
	}
	group, grouped := c.Output.AssetRef.GroupKey()
	if !grouped || group != d.GroupKey ||
		c.Output.IssuanceID != sdk.AssetID(p.Asset.ID()) ||
		c.Output.AssetType != sdk.AssetType(p.Asset.Genesis.Type) {

		return swapkit.ErrInvalid
	}
	assets, merkle, err := isolatedRoots(p)
	if err != nil {
		return err
	}
	previous := p.AnchorTx.TxOut[p.OutPoint().Index]
	if p.OutPoint().String() != d.Outpoint ||
		SDKPoint(c.Output.AnchorOutpoint) != p.OutPoint() ||
		c.Output.TaprootAssetRoot != assets ||
		c.Output.TaprootMerkleRoot != merkle ||
		c.Output.AnchorValueSat != previous.Value ||
		!bytes.Equal(p.Asset.ScriptKey.PubKey.SerializeCompressed(),
			c.Output.ScriptKey[:]) {

		return swapkit.ErrInvalid
	}
	dk, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: c.ClientKey, ServerKey: c.ServerKey,
		CSVExpiry: c.CSV,
	})
	if err != nil {
		return err
	}
	spend, err := dk.TimeoutSpend(assets)
	if err != nil {
		return err
	}
	if !bytes.Equal(spend.PkScript, previous.PkScript) {
		return swapkit.ErrInvalid
	}
	// Validate the exact asset witness, not just a caller's OP_TRUE label.
	err = c.Output.OPTrueSpend.Validate(c.Output.ScriptKey)
	if err != nil {
		return err
	}
	return nil
}
