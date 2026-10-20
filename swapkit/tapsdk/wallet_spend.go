package tapsdk

import (
	"bytes"
	"context"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

// rebuildObservedSpend reconstructs either an original deposit refund or an
// HTLC outcome from the exact mined witness. The SDK checks the Bitcoin
// witness against the agreed leaf. No wallet key, signing session or broadcast
// is needed, so this also recovers a proof from a permanently offline peer.
func (r *Driver) rebuildObservedSpend(ctx context.Context,
	prior sdk.CustomAnchorAssetOutputSummary, inputProof []byte,
	public [33]byte, spend *SwapAnchorSpendInfo, observed *wire.MsgTx) (
	[]byte, error) {

	if len(observed.TxIn) != 1 || len(observed.TxOut) != 1 ||
		!spendsLeaf(observed, prior, spend) {

		return nil, swapkit.ErrInvalid
	}
	p, err := lastProof(inputProof)
	if err != nil {
		return nil, err
	}
	value := observed.TxOut[0].Value
	recipient, err := walletOutput(public, 0, 0,
		prior.AssetRef, prior.Amount, value)
	if err != nil {
		return nil, err
	}
	anchor, err := anchorTemplate(p, recipient.Anchor.InternalKey,
		value, spend)
	if err != nil {
		return nil, err
	}
	pkg, err := r.commit(ctx, customRequest(assetInput(prior, inputProof),
		recipient, anchor, spend.SigningPlan(0)))
	if err != nil {
		return nil, err
	}
	packet, err := decodePSBT(pkg.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	if packet.UnsignedTx.TxHash() != observed.TxHash() {
		return nil, swapkit.ErrInvalid
	}
	requests, err := pkg.SigningRequests()
	if err != nil || len(requests.ScriptPath) != 1 {
		return nil, swapkit.ErrInvalid
	}
	witness := observed.TxIn[0].Witness
	final, err := pkg.ApplyScriptPathWitness(requests.ScriptPath[0].ID,
		witness[:len(witness)-2])
	if err != nil {
		return nil, err
	}
	if prior.OPTrueSpend == nil {
		return nil, swapkit.ErrInvalid
	}
	if err := r.declareOPTrue(ctx, prior.ScriptKey,
		prior.OPTrueSpend.WitnessStack()); err != nil {
		return nil, err
	}
	return r.logAndExport(ctx, transaction{Package: pkg,
		Final: final.AnchorPsbt}, true)
}

// verifyWalletSpend binds every recovery outcome to its agreed issuance,
// wallet recipient and exact Bitcoin path. The observed witness comes from
// the current chain rather than the peer's proof, which may omit witnesses.
func (r *Driver) verifyWalletSpend(ctx context.Context, raw []byte,
	prior sdk.CustomAnchorAssetOutputSummary, public [33]byte,
	spend *SwapAnchorSpendInfo, unspent bool, minHeight uint64) error {

	group, ok := prior.AssetRef.GroupKey()
	if !ok {
		return swapkit.ErrInvalid
	}
	p, err := r.verifyProof(ctx, raw, group, prior.Amount, unspent)
	if err != nil {
		return err
	}
	if sdk.AssetID(p.Asset.ID()) != prior.IssuanceID ||
		uint64(p.BlockHeight) < minHeight {

		return swapkit.ErrInvalid
	}
	expected, err := walletOutput(public, 0, 0,
		prior.AssetRef, prior.Amount, 330)
	if err != nil {
		return err
	}
	if !bytes.Equal(p.Asset.ScriptKey.PubKey.SerializeCompressed(),
		expected.Script.External.ScriptKey.PubKey[:]) ||
		!bytes.Equal(p.InclusionProof.InternalKey.SerializeCompressed(),
			public[:]) {

		return swapkit.ErrInvalid
	}
	block, err := r.cfg.Chain.Block(ctx, p.BlockHeight)
	if err != nil {
		return err
	}
	for _, tx := range block.Transactions {
		if tx.TxHash() != p.AnchorTx.TxHash() {
			continue
		}
		if !spendsLeaf(tx, prior, spend) {
			return swapkit.ErrInvalid
		}
		fetch := txscript.NewCannedPrevOutputFetcher(
			spend.PkScript, prior.AnchorValueSat,
		)
		hashes := txscript.NewTxSigHashes(tx, fetch)
		engine, err := txscript.NewEngine(spend.PkScript, tx, 0,
			txscript.StandardVerifyFlags, nil, hashes,
			prior.AnchorValueSat, fetch)
		if err != nil {
			return err
		}
		return engine.Execute()
	}
	return swapkit.ErrInvalid
}

// spendsLeaf checks policy selection before the SDK or Bitcoin VM verifies the
// unlocking data. Annexes and alternate leaves cannot authorize an outcome.
func spendsLeaf(tx *wire.MsgTx, prior sdk.CustomAnchorAssetOutputSummary,
	spend *SwapAnchorSpendInfo) bool {

	if len(tx.TxIn) != 1 || tx.TxIn[0].PreviousOutPoint !=
		sdkPoint(prior.AnchorOutpoint) {

		return false
	}
	w := tx.TxIn[0].Witness
	return len(w) >= 3 && bytes.Equal(w[len(w)-2], spend.LeafScript) &&
		bytes.Equal(w[len(w)-1], spend.ControlBlock)
}
