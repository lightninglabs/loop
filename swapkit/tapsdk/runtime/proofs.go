package runtime

import (
	"bytes"
	"context"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapsend"
)

// transaction is persisted before publication; retry never rebuilds it.
type transaction struct {
	Package *sdk.CustomAnchorTransferPackage
	Final   []byte
}

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
		return sdk.Hash{}, sdk.Hash{}, bridge.ErrInvalid
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
		return sdk.Hash{}, sdk.Hash{}, bridge.ErrInvalid
	}
	return sdk.Hash(assets), sdk.Hash(root), nil
}

func (r *Runtime) verifyProof(ctx context.Context, raw []byte,
	group [33]byte, amount uint64, unspent bool) (*proof.Proof, error) {

	p, err := lastProof(raw)
	if err != nil {
		return nil, err
	}
	if p.Asset.GroupKey == nil || p.Asset.Amount != amount ||
		!bytes.Equal(p.Asset.GroupKey.GroupPubKey.SerializeCompressed(),
			group[:]) {
		return nil, bridge.ErrInvalid
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
		return nil, bridge.ErrInvalid
	}
	if err := r.verifyConfirmation(ctx, p, unspent); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *Runtime) verifyConfirmation(ctx context.Context, p *proof.Proof,
	unspent bool) error {

	tip, err := r.cfg.Chain.Height(ctx)
	if err != nil {
		return err
	}
	if tip < p.BlockHeight || tip-p.BlockHeight+1 < r.cfg.MinConfs {
		return bridge.ErrPending
	}
	block, err := r.cfg.Chain.Block(ctx, p.BlockHeight)
	if err != nil {
		return err
	}
	if block.BlockHash() != p.BlockHeader.BlockHash() {
		return bridge.ErrPending
	}
	found := false
	for _, tx := range block.Transactions {
		found = found || tx.TxHash() == p.AnchorTx.TxHash()
	}
	if !found {
		return bridge.ErrInvalid
	}
	if unspent {
		available, err := r.cfg.Chain.Unspent(ctx, p.OutPoint())
		if err != nil {
			return err
		}
		if !available {
			return bridge.ErrPending
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

func finalTx(tx transaction) (*wire.MsgTx, error) {
	if tx.Package == nil {
		return nil, bridge.ErrInvalid
	}
	if err := tx.Package.VerifyFinalAnchorPSBT(tx.Final); err != nil {
		return nil, err
	}
	packet, err := decodePSBT(tx.Final)
	if err != nil {
		return nil, err
	}
	return psbt.Extract(packet)
}

// publish reconstructs proof delivery from persisted SDK suffixes and the
// current best chain. It does not depend on an in-memory tapd transfer event.
func (r *Runtime) publish(ctx context.Context, tx transaction,
	since uint32) ([]byte, error) {

	anchor, err := finalTx(tx)
	if err != nil {
		return nil, err
	}
	if len(tx.Package.Inputs) != 1 || len(tx.Package.Outputs) != 1 ||
		len(tx.Package.ProofUpdates) != 1 {
		return nil, bridge.ErrInvalid
	}
	output := tx.Package.Outputs[0]
	if output.OPTrueSpend != nil {
		if err := r.declareOPTrue(ctx, output.ScriptKey,
			output.OPTrueSpend.WitnessStack()); err != nil {
			return nil, err
		}
	}
	block, height, err := r.confirmation(ctx,
		anchor.TxHash(), since)
	if err == errScanIncomplete {
		return nil, bridge.ErrPending
	}
	if err == bridge.ErrPending {
		// Persist input lineage before tapd records the transfer.
		file := &sdk.ProofFile{
			RawProofFile: tx.Package.Inputs[0].ProofSource.Blob,
		}
		_, importErr := r.cfg.Wallet.ImportProofFile(ctx, file)
		if importErr != nil {
			return nil, importErr
		}
		_, publishErr := r.cfg.Wallet.PublishCustomAnchorTransfer(
			ctx, tx.Package, tx.Final,
		)
		if publishErr != nil {
			// Retry the same transaction after an ambiguous reply.
			if err := r.cfg.Chain.Publish(ctx, anchor); err != nil {
				return nil, err
			}
		}
		return nil, bridge.ErrPending
	}
	if err != nil {
		return nil, err
	}
	raw, err := confirmedProof(tx, block, height)
	if err != nil {
		return nil, err
	}
	group, ok := output.AssetRef.GroupKey()
	if !ok {
		return nil, bridge.ErrInvalid
	}
	_, err = r.verifyProof(ctx, raw, group, output.Amount, false)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// confirmedProof completes the single-input SDK transition using its actual
// mined block. This is a standard confirmed proof, including historical v0
// assets; the SDK's unconfirmed AssetProofPath has stricter v1-only rules.
// The caller verifies the resulting full lineage against tapd and the chain.
func confirmedProof(tx transaction, block *wire.MsgBlock,
	height uint32) ([]byte, error) {

	if tx.Package == nil || block == nil ||
		len(tx.Package.Inputs) != 1 ||
		len(tx.Package.ProofUpdates) != 1 {

		return nil, bridge.ErrInvalid
	}
	file, err := proof.DecodeFile(tx.Package.Inputs[0].ProofSource.Blob)
	if err != nil {
		return nil, err
	}
	var transition proof.Proof
	err = transition.Decode(bytes.NewReader(
		tx.Package.ProofUpdates[0].ProofBlob,
	))
	if err != nil {
		return nil, err
	}
	anchor, err := finalTx(tx)
	if err != nil {
		return nil, err
	}
	if transition.AnchorTx.TxHash() != anchor.TxHash() {
		return nil, bridge.ErrInvalid
	}
	index := -1
	for i, mined := range block.Transactions {
		if mined.TxHash() == anchor.TxHash() {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, bridge.ErrPending
	}
	merkle, err := proof.NewTxMerkleProof(block.Transactions, index)
	if err != nil {
		return nil, err
	}
	transition.BlockHeader = block.Header
	transition.BlockHeight = height
	transition.TxMerkleProof = *merkle
	if err := file.AppendProof(transition); err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	if err := file.Encode(&encoded); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func (r *Runtime) declareOPTrue(ctx context.Context, scriptKey sdk.PubKey,
	witness [][]byte) error {

	if len(witness) != 2 ||
		!bytes.Equal(witness[0], []byte{txscript.OP_TRUE}) {
		return bridge.ErrInvalid
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
