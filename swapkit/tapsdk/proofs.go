package tapsdk

import (
	"bytes"
	"context"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/loop/internal/assetproof"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
)

// LastProof decodes the final transition in a native asset proof file.
func LastProof(raw []byte) (*proof.Proof, error) {
	if err := assetpolicy.CheckProofFileSize(raw); err != nil {
		return nil, err
	}
	file, err := proof.DecodeFile(raw)
	if err != nil {
		return nil, err
	}
	p, err := file.LastProof()
	if err != nil {
		return nil, err
	}
	if err := p.Asset.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func isolatedRoots(p *proof.Proof) (sdk.Hash, sdk.Hash, error) {
	if p == nil || p.InclusionProof.CommitmentProof == nil ||
		p.InclusionProof.InternalKey == nil {

		return sdk.Hash{}, sdk.Hash{}, swapkit.ErrInvalid
	}
	tc, err := assetproof.VerifyCompleteAnchor(p)
	if err != nil {
		return sdk.Hash{}, sdk.Hash{}, err
	}
	return commitmentRoots(p, tc)
}

func proofRoots(p *proof.Proof) (sdk.Hash, sdk.Hash, error) {
	if p == nil {
		return sdk.Hash{}, sdk.Hash{}, swapkit.ErrInvalid
	}
	tc, err := p.VerifyProofs()
	if err != nil {
		return sdk.Hash{}, sdk.Hash{}, err
	}
	return commitmentRoots(p, tc)
}

func commitmentRoots(
	p *proof.Proof,
	tc *commitment.TapCommitment,
) (sdk.Hash, sdk.Hash, error) {

	_, sibling, err := commitment.MaybeEncodeTapscriptPreimage(
		p.InclusionProof.CommitmentProof.TapSiblingPreimage)
	if err != nil {
		return sdk.Hash{}, sdk.Hash{}, err
	}
	root, assets := tc.TapscriptRoot(sibling), tc.TapscriptRoot(nil)
	script, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootOutputKey(
			p.InclusionProof.InternalKey,
			root[:],
		),
	)
	index := p.OutPoint().Index
	if err != nil || index >= uint32(len(p.AnchorTx.TxOut)) ||
		!bytes.Equal(script, p.AnchorTx.TxOut[index].PkScript) {

		return sdk.Hash{}, sdk.Hash{}, swapkit.ErrInvalid
	}
	return sdk.Hash(assets), sdk.Hash(root), nil
}

// checkConfirmation requires current-chain inclusion and optional spendability.
func checkConfirmation(ctx context.Context, chain Chain,
	minConfs uint32, p *proof.Proof,
	unspent bool) error {

	if chain == nil || p == nil || minConfs == 0 || p.BlockHeight == 0 ||
		p.OutPoint().Index >= uint32(len(p.AnchorTx.TxOut)) {

		return swapkit.ErrInvalid
	}
	tip, err := chain.Height(ctx)
	if err != nil {
		return err
	}
	if tip < p.BlockHeight || tip-p.BlockHeight+1 < minConfs {
		return swapkit.ErrPending
	}
	hash, err := chain.BlockHash(ctx, p.BlockHeight)
	if err != nil {
		return err
	}
	if hash != p.BlockHeader.BlockHash() {
		return swapkit.ErrPending
	}
	if len(p.TxMerkleProof.Nodes) != len(p.TxMerkleProof.Bits) ||
		!p.TxMerkleProof.Verify(&p.AnchorTx, p.BlockHeader.MerkleRoot) {

		return swapkit.ErrInvalid
	}
	if unspent {
		available, err := chain.Unspent(ctx, p.OutPoint())
		if err != nil {
			return err
		}
		if !available {
			return swapkit.ErrPending
		}
	}
	return nil
}

// DecodePSBT reads the native Bitcoin PSBT carried by an SDK transfer.
func DecodePSBT(raw []byte) (*psbt.Packet, error) {
	return psbt.NewFromRawBytes(bytes.NewReader(raw), false)
}

// SDKPoint converts a native SDK outpoint to the Bitcoin wire type.
func SDKPoint(p sdk.Outpoint) wire.OutPoint {
	return wire.OutPoint{Hash: chainhash.Hash(p.Txid), Index: p.Index}
}

// checkAsset checks negotiated native quantity and group membership.
func checkAsset(p *proof.Proof, group [33]byte, amount uint64) error {
	if p == nil || p.Asset.GroupKey == nil || p.Asset.Amount != amount ||
		p.Asset.LockTime != 0 || p.Asset.RelativeLockTime != 0 ||
		!sameGroupKeys(
			p.Asset.GroupKey.GroupPubKey.SerializeCompressed(),
			group[:],
		) {

		return swapkit.ErrInvalid
	}
	return nil
}

// inspectProof checks Loop policy and the current chain before SDK Build.
// SDK Build is responsible for generic proof verification on building paths.
func inspectProof(ctx context.Context, chain Chain, minConfs uint32, raw []byte,
	group [33]byte, amount uint64, unspent bool) (*proof.Proof, error) {

	p, err := LastProof(raw)
	if err != nil {
		return nil, err
	}
	if err := checkAsset(p, group, amount); err != nil {
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
