package tapsdk

import (
	"bytes"
	"context"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

// rebuildObservedSpend reconstructs an SDK transfer from a mined witness.
// It neither signs a wallet transaction nor broadcasts the observed spend.
func rebuildObservedSpend(
	ctx context.Context,
	wallet *sdk.Wallet,
	chain Chain,
	minConfs uint32,
	prior sdk.CustomAnchorAssetOutputSummary,
	inputProof []byte,
	public [33]byte,
	spend *swapkit.SwapAnchorSpendInfo,
	observed *wire.MsgTx,
) (
	[]byte, error) {

	if len(observed.TxIn) != 1 || len(observed.TxOut) != 1 ||
		!spendsLeaf(observed, prior, spend) {

		return nil, swapkit.ErrInvalid
	}
	// Reconstruction replays the mined script-path stack minus its leaf and
	// control block. A BIP-341 annex would shift those two elements, so the
	// reconstructed witness would carry the wrong stack. An observed annex
	// spend is rare and not reconstructable here, so reject it up front
	// rather than mis-slice it.
	if len(observed.TxIn[0].Witness) !=
		len(witnessWithoutAnnex(observed.TxIn[0].Witness)) {

		return nil, swapkit.ErrInvalid
	}
	p, err := LastProof(inputProof)
	if err != nil {
		return nil, err
	}
	value := observed.TxOut[0].Value
	recipient, err := walletOutput(public, 0, 0,
		prior.AssetRef, prior.Amount, value)
	if err != nil {
		return nil, err
	}
	anchor, err := AnchorTemplate(p, recipient.Anchor.InternalKey,
		value, spend)
	if err != nil {
		return nil, err
	}
	// Recovery reproduces the mined transaction, including older non-RBF
	// refunds. New templates use the current default sequence instead.
	packet, err := DecodePSBT(anchor)
	if err != nil {
		return nil, err
	}
	packet.UnsignedTx.Version = observed.Version
	packet.UnsignedTx.LockTime = observed.LockTime
	packet.UnsignedTx.TxIn[0].Sequence = observed.TxIn[0].Sequence
	anchor, err = EncodePSBT(packet)
	if err != nil {
		return nil, err
	}
	input, err := AssetInput(prior, inputProof)
	if err != nil {
		return nil, err
	}
	request := CustomRequest(input,
		recipient, anchor, ScriptSigningPlan(spend, 0))
	pkg, err := commit(ctx, wallet, chain, minConfs, request)
	if err != nil {
		return nil, err
	}
	packet, err = DecodePSBT(pkg.AnchorPsbt)
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
	final, err = pkg.WithFinalAnchorPSBT(final.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	return LogAndExport(ctx, wallet, chain, minConfs, final, true)
}

// VerifyWalletSpend checks issuance, wallet recipient and Bitcoin leaf.
func VerifyWalletSpend(
	ctx context.Context,
	wallet *sdk.Wallet,
	chain Chain,
	minConfs uint32,
	raw []byte,
	prior sdk.CustomAnchorAssetOutputSummary,
	public [33]byte,
	spend *swapkit.SwapAnchorSpendInfo,
	unspent bool,
	minHeight uint64,
) error {

	group, ok := prior.AssetRef.GroupKey()
	if !ok {
		return swapkit.ErrInvalid
	}
	p, err := verifyProof(
		ctx, wallet, chain, minConfs, raw, group, prior.Amount, unspent,
	)
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
	block, err := chain.Block(ctx, p.BlockHeight)
	if err != nil {
		return err
	}
	// verifyProof confirmed the proof against the chain, but a reorg between
	// that check and this re-read would replace the block at this height.
	// Without the agreed transaction the loop below falls through to
	// ErrInvalid; report the reorg as retryable instead, matching every other
	// stale-evidence path.
	if block.BlockHash() != p.BlockHeader.BlockHash() {
		return swapkit.ErrPending
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

func spendsLeaf(tx *wire.MsgTx, prior sdk.CustomAnchorAssetOutputSummary,
	spend *swapkit.SwapAnchorSpendInfo) bool {

	if len(tx.TxIn) != 1 || tx.TxIn[0].PreviousOutPoint !=
		SDKPoint(prior.AnchorOutpoint) {

		return false
	}
	w := witnessWithoutAnnex(tx.TxIn[0].Witness)
	return len(w) >= 3 && bytes.Equal(w[len(w)-2], spend.LeafScript) &&
		bytes.Equal(w[len(w)-1], spend.ControlBlock)
}

// witnessWithoutAnnex drops a BIP-341 annex. A taproot witness carries an annex
// when its final element begins with the annex tag and at least one other
// element precedes it. The annex is not part of the script-path stack, so the
// agreed leaf and control block are the last two elements once it is removed.
func witnessWithoutAnnex(w [][]byte) [][]byte {
	if len(w) >= 2 && len(w[len(w)-1]) > 0 &&
		w[len(w)-1][0] == txscript.TaprootAnnexTag {

		return w[:len(w)-1]
	}
	return w
}
