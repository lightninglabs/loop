package tapsdk

import (
	"bytes"
	"context"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapsend"
)

func lastProof(raw []byte) (*proof.Proof, error) {
	file, err := proof.DecodeFile(raw)
	if err != nil {
		return nil, err
	}
	return file.LastProof()
}

// isolatedRoots proves absence of additional live assets by reconstructing
// the entire anchor commitment from one asset and its disclosed alt leaves.
func isolatedRoots(p *proof.Proof) (sdk.Hash, sdk.Hash, error) {
	if p == nil || p.InclusionProof.CommitmentProof == nil ||
		p.InclusionProof.InternalKey == nil {

		return sdk.Hash{}, sdk.Hash{}, swapkit.ErrInvalid
	}
	a := p.Asset.Copy()
	if a.HasSplitCommitmentWitness() {
		a.PrevWitnesses[0].SplitCommitment = nil
	}
	ac, err := commitment.NewAssetCommitment(a)
	if err != nil {
		return sdk.Hash{}, sdk.Hash{}, err
	}
	version := p.InclusionProof.CommitmentProof.TaprootAssetProof.Version
	tc, err := commitment.NewTapCommitment(&version, ac)
	if err != nil {
		return sdk.Hash{}, sdk.Hash{}, err
	}
	if err := asset.ValidAltLeaves(p.AltLeaves); err != nil {
		return sdk.Hash{}, sdk.Hash{}, err
	}
	if err := tc.MergeAltLeaves(p.AltLeaves); err != nil {
		return sdk.Hash{}, sdk.Hash{}, err
	}
	script, root, assets, err := tapsend.AnchorOutputScript(
		p.InclusionProof.InternalKey,
		p.InclusionProof.CommitmentProof.TapSiblingPreimage, tc,
	)
	index := p.OutPoint().Index
	if err != nil || index >= uint32(len(p.AnchorTx.TxOut)) ||
		!bytes.Equal(script, p.AnchorTx.TxOut[index].PkScript) {

		return sdk.Hash{}, sdk.Hash{}, swapkit.ErrInvalid
	}
	return sdk.Hash(assets), sdk.Hash(root), nil
}

func (r *Driver) verifyProof(ctx context.Context, raw []byte,
	group [33]byte, amount uint64, unspent bool) (*proof.Proof, error) {

	p, err := lastProof(raw)
	if err != nil {
		return nil, err
	}
	if p.Asset.GroupKey == nil || p.Asset.Amount != amount ||
		!bytes.Equal(p.Asset.GroupKey.GroupPubKey.SerializeCompressed(),
			group[:]) {

		return nil, swapkit.ErrInvalid
	}
	if _, _, err := isolatedRoots(p); err != nil {
		return nil, err
	}
	// Classify a relocated tip as pending even if tapd would first reject
	// the old header. Repeat current-chain checks after full verification
	// so a slow verifier cannot authorize stale confirmation or UTXO data.
	if err := r.verifyConfirmation(ctx, p, unspent); err != nil {
		return nil, err
	}
	verified, err := r.cfg.SDK.VerifyProof(ctx, raw)
	if err != nil {
		return nil, err
	}
	if verified == nil || !verified.Valid {
		return nil, swapkit.ErrInvalid
	}
	if err := r.verifyConfirmation(ctx, p, unspent); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *Driver) verifyConfirmation(ctx context.Context, p *proof.Proof,
	unspent bool) error {

	tip, err := r.cfg.Chain.Height(ctx)
	if err != nil {
		return err
	}
	if tip < p.BlockHeight || tip-p.BlockHeight+1 < r.cfg.MinConfs {
		return swapkit.ErrPending
	}
	block, err := r.cfg.Chain.Block(ctx, p.BlockHeight)
	if err != nil {
		return err
	}
	if block.BlockHash() != p.BlockHeader.BlockHash() {
		return swapkit.ErrPending
	}
	found := false
	for _, tx := range block.Transactions {
		found = found || tx.TxHash() == p.AnchorTx.TxHash()
	}
	if !found {
		return swapkit.ErrInvalid
	}
	if unspent {
		available, err := r.cfg.Chain.Unspent(ctx, p.OutPoint())
		if err != nil {
			return err
		}
		if !available {
			return swapkit.ErrPending
		}
	}
	return nil
}

func decodePSBT(raw []byte) (*psbt.Packet, error) {
	return psbt.NewFromRawBytes(bytes.NewReader(raw), false)
}

func encodePSBT(p *psbt.Packet) ([]byte, error) {
	var b bytes.Buffer
	err := p.Serialize(&b)
	return b.Bytes(), err
}

// decodePackage uses the SDK's validated native package encoding.
func decodePackage(raw []byte) (*sdk.CustomAnchorTransferPackage, error) {
	var pkg sdk.CustomAnchorTransferPackage
	if err := pkg.UnmarshalBinary(raw); err != nil {
		return nil, err
	}
	return &pkg, nil
}

func decodeFinalPackage(raw []byte) (*sdk.CustomAnchorTransferPackage, error) {
	pkg, err := decodePackage(raw)
	if err != nil {
		return nil, err
	}
	return pkg.WithFinalAnchorPSBT(pkg.AnchorPsbt)
}

// bindFinalPackage retains the approved package and accepts only its fully
// finalized Bitcoin witness. The committed digest binds all SDK metadata.
func bindFinalPackage(approved *sdk.CustomAnchorTransferPackage,
	raw []byte) (*sdk.CustomAnchorTransferPackage, error) {

	proposed, err := decodePackage(raw)
	if err != nil {
		return nil, err
	}
	if approved == nil || proposed.CommittedPackageDigest !=
		approved.CommittedPackageDigest {

		return nil, swapkit.ErrInvalid
	}
	return approved.WithFinalAnchorPSBT(proposed.AnchorPsbt)
}

// publish hands the finalized package to tapd, which journals publication and
// rebuilds confirmed proof files after restart. Export always uses the exact
// issuance, script key and outpoint, including when a publish reply was lost.
func (r *Driver) publish(ctx context.Context,
	pkg *sdk.CustomAnchorTransferPackage) ([]byte, error) {

	return r.logAndExport(ctx, pkg, false)
}

// logAndExport can also record a peer's already-mined transaction without
// broadcasting it. The same tapd transfer journal owns both recovery paths.
func (r *Driver) logAndExport(ctx context.Context,
	pkg *sdk.CustomAnchorTransferPackage, observed bool) ([]byte, error) {

	if len(pkg.Inputs) != 1 || len(pkg.Outputs) != 1 ||
		len(pkg.ProofUpdates) != 1 {

		return nil, swapkit.ErrInvalid
	}
	out := pkg.Outputs[0]
	export := func() ([]byte, error) {
		file, err := r.cfg.Wallet.ExportProofFile(ctx,
			sdk.AssetRefFromAssetID(out.IssuanceID), out.ScriptKey,
			&out.AnchorOutpoint)
		if err != nil {
			return nil, err
		}
		if file == nil {
			return nil, swapkit.ErrInvalid
		}
		p, err := lastProof(file.RawProofFile)
		if err != nil {
			return nil, err
		}
		if sdk.AssetID(p.Asset.ID()) != out.IssuanceID ||
			p.OutPoint() != sdkPoint(out.AnchorOutpoint) ||
			!bytes.Equal(p.Asset.ScriptKey.PubKey.SerializeCompressed(),
				out.ScriptKey[:]) {

			return nil, swapkit.ErrInvalid
		}
		// The caller verifies the proof with its contract exactly once.
		// Here a current confirmation distinguishes archive lag from success.
		if err := r.verifyConfirmation(ctx, p, false); err != nil {
			return nil, err
		}
		return file.RawProofFile, nil
	}
	if raw, err := export(); err == nil {
		return raw, nil
	}
	if out.OPTrueSpend != nil {
		if err := r.declareOPTrue(ctx, out.ScriptKey,
			out.OPTrueSpend.WitnessStack()); err != nil {
			return nil, err
		}
	}
	err := r.importProof(ctx, pkg.Inputs[0].ProofSource.Blob)
	if err != nil {
		return nil, err
	}
	if observed {
		pkg = pkg.Clone()
		pkg.Publish.SkipAnchorTxBroadcast = true
		pkg.Publish.ExternalBroadcast = true
		pkg, err = pkg.Seal()
		if err != nil {
			return nil, err
		}
	}
	_, err = r.cfg.Wallet.PublishCustomAnchorTransfer(ctx, pkg, pkg.AnchorPsbt)
	if err != nil {
		// A lost reply can follow a durable tapd publication. The next
		// invocation checks the proof archive before retrying the log.
		return nil, err
	}
	if raw, err := export(); err == nil {
		return raw, nil
	}
	return nil, swapkit.ErrPending
}

// importProof restores missing lineage. Tapd versions that reject duplicate
// imports still permit retries through the exact proof archive locator.
func (r *Driver) importProof(ctx context.Context, raw []byte) error {
	p, err := lastProof(raw)
	if err != nil {
		return err
	}
	key, err := sdk.ParsePubKey(p.Asset.ScriptKey.PubKey.SerializeCompressed())
	if err != nil {
		return err
	}
	point := sdk.Outpoint{Txid: sdk.Hash(p.OutPoint().Hash),
		Index: p.OutPoint().Index}
	file, err := r.cfg.Wallet.ExportProofFile(ctx,
		sdk.AssetRefFromAssetID(sdk.AssetID(p.Asset.ID())), key, &point)
	if err == nil && file != nil {
		existing, err := lastProof(file.RawProofFile)
		if err != nil {
			return err
		}
		if existing.OutPoint() != p.OutPoint() ||
			existing.Asset.ID() != p.Asset.ID() ||
			existing.Asset.Amount != p.Asset.Amount ||
			!existing.Asset.ScriptKey.PubKey.IsEqual(p.Asset.ScriptKey.PubKey) {

			return swapkit.ErrInvalid
		}
		return nil
	}
	_, err = r.cfg.Wallet.ImportProofFile(ctx,
		&sdk.ProofFile{RawProofFile: raw})
	return err
}

func (r *Driver) declareOPTrue(ctx context.Context, scriptKey sdk.PubKey,
	witness [][]byte) error {

	if len(witness) != 2 ||
		!bytes.Equal(witness[0], []byte{txscript.OP_TRUE}) {

		return swapkit.ErrInvalid
	}
	control, err := txscript.ParseControlBlock(witness[1])
	if err != nil {
		return err
	}
	internal, err := sdk.ParsePubKey(
		control.InternalKey.SerializeCompressed(),
	)
	if err != nil {
		return err
	}
	_, err = r.cfg.SDK.DeclareScriptKey(ctx, &sdk.DeclareScriptKeyRequest{
		ScriptKey: sdk.ScriptKey{PubKey: scriptKey,
			KeyDesc:  sdk.KeyDescriptor{RawKeyBytes: internal},
			TapTweak: control.RootHash(witness[0])},
	})
	return err
}

func sdkPoint(p sdk.Outpoint) wire.OutPoint {
	return wire.OutPoint{Hash: chainhash.Hash(p.Txid), Index: p.Index}
}

// resolveOutcomeProof verifies a peer hint or independently discovers the
// outcome. Neither path bypasses current-chain and contract validation.
func resolveOutcomeProof(ctx context.Context, supplied []byte,
	verify func(context.Context, []byte) error,
	discover func(context.Context) ([]byte, error)) ([]byte, error) {

	if len(supplied) != 0 {
		return supplied, verify(ctx, supplied)
	}
	raw, err := discover(ctx)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, swapkit.ErrPending
	}
	return raw, verify(ctx, raw)
}
