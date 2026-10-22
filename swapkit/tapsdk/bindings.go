package tapsdk

import (
	"bytes"
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/loop/internal/assetproof"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
)

// InputBinding contains independently agreed terms for one exact issuance and
// anchor. A nil GroupKey requires an ungrouped asset. Anchor and OPTrue describe
// contract inputs; Spend optionally fixes a unilateral Bitcoin path. These are
// operation-local policy inputs, not another transfer or persistence format.
type InputBinding struct {
	ID             string
	IssuanceID     sdk.AssetID
	GroupKey       *sdk.PubKey
	Amount         uint64
	ScriptKey      sdk.PubKey
	AnchorOutpoint sdk.Outpoint
	Anchor         *swapkit.AnchorPlan
	OPTrue         *sdk.CustomAssetOPTrueSpendInfo
	Spend          *swapkit.SwapAnchorSpendInfo
}

func (b InputBinding) clone() InputBinding {
	if b.GroupKey != nil {
		b.GroupKey = new(*b.GroupKey)
	}
	if b.Anchor != nil {
		b.Anchor = cloneAnchor(*b.Anchor)
	}
	b.OPTrue = b.OPTrue.Clone()
	if b.Spend != nil {
		spend := *b.Spend
		spend.PkScript = bytes.Clone(spend.PkScript)
		spend.LeafScript = bytes.Clone(spend.LeafScript)
		spend.ControlBlock = bytes.Clone(spend.ControlBlock)
		b.Spend = &spend
	}
	return b
}

func cloneAnchor(a swapkit.AnchorPlan) *swapkit.AnchorPlan {
	leaves := make([][]byte, len(a.Leaves))
	for i := range a.Leaves {
		leaves[i] = bytes.Clone(a.Leaves[i])
	}
	a.Leaves = leaves
	return &a
}

func (b InputBinding) validate(p *proof.Proof) error {
	if p == nil || b.Amount > math.MaxInt64 {
		return swapkit.ErrInvalid
	}
	if err := p.Asset.Validate(); err != nil {
		return err
	}
	if b.ID == "" || b.IssuanceID == (sdk.AssetID{}) || b.Amount == 0 ||
		sdk.AssetID(p.Asset.ID()) != b.IssuanceID ||
		p.Asset.Amount != b.Amount ||
		p.OutPoint() != SDKPoint(b.AnchorOutpoint) ||
		!bytes.Equal(p.Asset.ScriptKey.PubKey.SerializeCompressed()[1:], b.ScriptKey[1:]) {

		return fmt.Errorf(
			"%w: asset differs from agreed input",
			swapkit.ErrInvalid,
		)
	}
	if _, err := btcec.ParsePubKey(b.ScriptKey[:]); err != nil {
		return err
	}
	group := p.Asset.GroupKey
	if (group == nil) != (b.GroupKey == nil) || (group != nil &&
		!sameGroupKeys(group.GroupPubKey.SerializeCompressed(), b.GroupKey[:])) {

		return fmt.Errorf(
			"%w: asset differs from agreed group",
			swapkit.ErrInvalid,
		)
	}
	if p.Asset.ScriptVersion != asset.ScriptV0 || p.Asset.LockTime != 0 ||
		p.Asset.RelativeLockTime != 0 {

		return fmt.Errorf(
			"%w: contract asset has unsupported timelocks",
			swapkit.ErrInvalid,
		)
	}
	if b.Anchor == nil {
		if b.Spend != nil || b.OPTrue != nil {
			return fmt.Errorf(
				"wallet input cannot select a contract spend",
			)
		}
		return nil
	}
	if _, err := OPTrueSpendInfo(sdk.CustomAnchorAssetOutputSummary{
		ScriptKey: b.ScriptKey, ScriptMode: sdk.CustomAssetScriptExternal, OPTrueSpend: b.OPTrue,
	}); err != nil {
		return err
	}
	assets, root, err := proofRoots(p)
	if err != nil {
		return err
	}
	script, err := anchorScript(*b.Anchor, assets)
	if err != nil {
		return err
	}
	point := p.OutPoint()
	if point.Index >= uint32(len(p.AnchorTx.TxOut)) ||
		p.AnchorTx.TxOut[point.Index] == nil || !bytes.Equal(script, p.AnchorTx.TxOut[point.Index].PkScript) {

		return fmt.Errorf(
			"%w: input anchor differs from contract",
			swapkit.ErrInvalid,
		)
	}
	if b.Spend != nil &&
		(!bytes.Equal(b.Spend.PkScript, script) || sdk.Hash(b.Spend.TaprootMerkleRoot) != root) {

		return fmt.Errorf(
			"%w: spend differs from contract root",
			swapkit.ErrInvalid,
		)
	}
	return nil
}

func anchorScript(a swapkit.AnchorPlan, assets sdk.Hash) ([]byte, error) {
	internal, err := btcec.ParsePubKey(a.InternalKey[:])
	if err != nil {
		return nil, err
	}
	root := chainhash.Hash(assets)
	if len(a.Leaves) > 0 {
		leaves := make([]txscript.TapLeaf, len(a.Leaves))
		for i, script := range a.Leaves {
			if len(script) == 0 {
				return nil, fmt.Errorf("empty contract leaf")
			}
			leaves[i] = txscript.NewBaseTapLeaf(script)
		}
		sibling := txscript.AssembleTaprootScriptTree(
			leaves...).RootNode.TapHash()
		root = asset.TapBranchHash(chainhash.Hash(assets), sibling)
	}
	return txscript.PayToTaprootScript(
		txscript.ComputeTaprootOutputKey(internal, root[:]),
	)
}

// InspectInputProof checks exact contract terms and current-chain policy before
// native SDK Build verifies the full lineage. It does not authorize a spend.
func InspectInputProof(
	ctx context.Context,
	wallet *sdk.Wallet,
	chain Chain,
	minConfs uint32,
	raw []byte,
	binding InputBinding,
	unspent bool,
) (*proof.Proof, error) {

	if err := assetpolicy.CheckProofFileSize(raw); err != nil {
		return nil, err
	}
	raw = bytes.Clone(raw)
	binding = binding.clone()
	if err := checkProvenance(ctx, wallet, raw); err != nil {
		return nil, err
	}
	p, err := LastProof(raw)
	if err != nil {
		return nil, err
	}
	if err := binding.validate(p); err != nil {
		return nil, err
	}
	if _, _, err := isolatedRoots(p); err != nil {
		return nil, err
	}
	if err := checkConfirmation(ctx, chain, minConfs, p, unspent); err != nil {
		return nil, err
	}
	return p, nil
}

// VerifyInputProof validates negotiated input policy and asks authenticated
// tapd to verify lineage exactly once, bracketed by current-chain checks.
func VerifyInputProof(
	ctx context.Context,
	wallet *sdk.Wallet,
	chain Chain,
	minConfs uint32,
	raw []byte,
	binding InputBinding,
	unspent bool,
) (*proof.Proof, error) {

	if err := assetpolicy.CheckProofFileSize(raw); err != nil {
		return nil, err
	}
	raw = bytes.Clone(raw)
	p, err := InspectInputProof(
		ctx,
		wallet,
		chain,
		minConfs,
		raw,
		binding,
		unspent,
	)
	if err != nil {
		return nil, err
	}
	result, err := wallet.Client().VerifyProof(ctx, bytes.Clone(raw))
	if err != nil {
		return nil, err
	}
	if err := checkVerifiedSummary(result, p); err != nil {
		return nil, err
	}
	if err := checkConfirmation(ctx, chain, minConfs, p, unspent); err != nil {
		return nil, err
	}
	return p, nil
}

// InspectInputProofs checks all active and passive bindings as a single anchor
// inventory. Unlike the single-input helper, it accepts disclosed co-anchored
// assets when their union opens the complete commitment. Include every active
// and passive proof and binding; omitted assets fail closed before SDK Build.
func InspectInputProofs(
	ctx context.Context,
	wallet *sdk.Wallet,
	chain Chain,
	minConfs uint32,
	raws [][]byte,
	bindings []InputBinding,
	unspent bool,
) ([]*proof.Proof, error) {

	if len(raws) == 0 || len(raws) != len(bindings) {
		return nil, swapkit.ErrInvalid
	}
	var total int
	for _, raw := range raws {
		if len(raw) > assetpolicy.MaxProvenanceBytes-total {
			return nil, fmt.Errorf(
				"proof inventory exceeds size budget",
			)
		}
		total += len(raw)
	}
	snapshots := make([]InputBinding, len(bindings))
	copied := make([][]byte, len(raws))
	for i, binding := range bindings {
		snapshots[i] = binding.clone()
		copied[i] = bytes.Clone(raws[i])
	}
	bindings, raws = snapshots, copied
	params, err := nodeParams(ctx, wallet)
	if err != nil {
		return nil, err
	}
	proofs := make([]*proof.Proof, len(raws))
	ids := make(map[string]bool)
	for i, raw := range raws {
		binding := bindings[i].clone()
		if ids[binding.ID] {
			return nil, fmt.Errorf("duplicate input binding")
		}
		ids[binding.ID] = true
		if err := checkProvenanceWithParams(ctx, raw, params); err != nil {
			return nil, err
		}
		p, err := LastProof(raw)
		if err != nil {
			return nil, err
		}
		if err := binding.validate(p); err != nil {
			return nil, err
		}
		if err := checkConfirmation(ctx, chain, minConfs, p, unspent); err != nil {
			return nil, err
		}
		proofs[i] = p
	}
	if err := assetproof.VerifyCompleteAnchors(proofs); err != nil {
		return nil, err
	}
	return proofs, nil
}
