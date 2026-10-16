// Package sweep validates asset transfers before signing their Bitcoin anchor.
package sweep

import (
	"bytes"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/internal/policy"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightninglabs/taproot-assets/vm"
)

// Transfer is the caller-approved asset transfer and Bitcoin outputs to anchor.
// The caller must construct or check the destinations, amounts and anchor keys
// independently of an untrusted Bitcoin PSBT. All active, change and passive
// assets must be included, with their virtual witnesses complete. Validation
// does not establish ownership of destination keys, proof provenance or chain
// confirmation status. Use NewTransfer to record the approved Bitcoin outputs.
type Transfer struct {
	Packets []*tappsbt.VPacket

	// PrunedAssets contains only existing tombstones and provable burns
	// omitted from the virtual packets, keyed by their input anchor outpoint.
	PrunedAssets map[wire.OutPoint][]*asset.Asset

	bitcoinOutputs []*wire.TxOut
}

// NewTransfer records a caller-approved asset transfer and the complete ordered
// Bitcoin output list, including Bitcoin-only change. The caller must approve
// these outputs independently of an untrusted PSBT, after funding and fee
// selection. Output values and scripts are copied so later changes require a
// new approval. Virtual packets remain caller-owned and are checked by Validate.
func NewTransfer(packets []*tappsbt.VPacket,
	bitcoinOutputs []*wire.TxOut) *Transfer {

	outputs := make([]*wire.TxOut, len(bitcoinOutputs))
	for idx, out := range bitcoinOutputs {
		if out != nil {
			outputs[idx] = &wire.TxOut{
				Value:    out.Value,
				PkScript: bytes.Clone(out.PkScript),
			}
		}
	}

	return &Transfer{Packets: packets, bitcoinOutputs: outputs}
}

// Validate binds the proof-selected asset and the complete input commitments to
// valid virtual transitions, then binds every resulting asset commitment to the
// exact Bitcoin outputs being signed. New outputs must include the STXO and
// spender leaves required by Taproot Assets v0.8.5, even for legacy inputs.
// Bitcoin-only Taproot outputs must supply PSBT internal-key and, if present,
// tap-tree metadata that proves they contain no asset commitment.
// Bitcoin locktime and input sequences must enforce every output asset's
// timelocks, so the anchor cannot confirm before its transitions are valid.
// Neither the PSBT nor Transfer is mutated.
func (t *Transfer) Validate(p *proof.Proof, btc *psbt.Packet) error {
	if t == nil || len(t.Packets) == 0 {
		return fmt.Errorf("asset transfer packets are required")
	}
	if p == nil || btc == nil || btc.UnsignedTx == nil {
		return fmt.Errorf("asset proof and Bitcoin PSBT are required")
	}
	if err := policy.ValidateTransitionVersion(p.Version); err != nil {
		return err
	}
	tx := btc.UnsignedTx
	if len(tx.TxIn) != len(btc.Inputs) ||
		len(tx.TxOut) != len(btc.Outputs) {

		return fmt.Errorf("Bitcoin PSBT metadata is incomplete")
	}
	btcInputs := make(map[wire.OutPoint]bool, len(tx.TxIn))
	for _, in := range tx.TxIn {
		if in == nil || btcInputs[in.PreviousOutPoint] {
			return fmt.Errorf("nil or duplicate Bitcoin input")
		}
		btcInputs[in.PreviousOutPoint] = true
	}
	for _, out := range tx.TxOut {
		if out == nil || out.Value < 0 {
			return fmt.Errorf("invalid Bitcoin output")
		}
	}
	if len(t.bitcoinOutputs) == 0 {
		return fmt.Errorf("Bitcoin output approval is required")
	}
	if len(tx.TxOut) != len(t.bitcoinOutputs) {
		return fmt.Errorf("Bitcoin output count does not match approval")
	}
	for idx, out := range tx.TxOut {
		approved := t.bitcoinOutputs[idx]
		if approved == nil || out.Value != approved.Value ||
			!bytes.Equal(out.PkScript, approved.PkScript) {

			return fmt.Errorf("Bitcoin output %d does not match approval",
				idx)
		}
	}

	var found bool
	anchors := make(map[wire.OutPoint]bool)
	anchorPackets := make([]*tappsbt.VPacket, len(t.Packets))
	proofAsset := anchorAsset(&p.Asset)
	for idx, pkt := range t.Packets {
		if err := validatePacket(pkt, len(tx.TxOut)); err != nil {
			return fmt.Errorf("asset transfer %d: %w", idx, err)
		}
		if err := validateAnchorTimeLocks(pkt, tx); err != nil {
			return fmt.Errorf("asset timelocks for transfer %d: %w",
				idx, err)
		}
		anchorPackets[idx] = &tappsbt.VPacket{}
		for _, in := range pkt.Inputs {
			anchorInput := in.Copy()
			trimSplitWitness(anchorInput.Asset())
			anchorPackets[idx].Inputs = append(
				anchorPackets[idx].Inputs, anchorInput,
			)
			anchors[in.PrevID.OutPoint] = true
			if in.PrevID.OutPoint == p.OutPoint() &&
				anchorInput.Asset().
					DeepEqualAllowSegWitIgnoreTxWitness(proofAsset) {

				found = true
			}
		}
	}
	if !found {
		return fmt.Errorf("asset transfer does not spend proof asset")
	}
	prunedAssets := make(map[wire.OutPoint][]*asset.Asset)
	for outpoint, pruned := range t.PrunedAssets {
		if !anchors[outpoint] {
			return fmt.Errorf("pruned assets have no input anchor")
		}
		for _, a := range pruned {
			if a == nil || a.ScriptKey.PubKey == nil ||
				(!a.IsUnSpendable() && !a.IsBurn()) {

				return fmt.Errorf("cannot prune spendable assets")
			}
			prunedAssets[outpoint] = append(
				prunedAssets[outpoint], anchorAsset(a),
			)
		}
	}
	if err := tapsend.ValidateVPacketVersions(t.Packets); err != nil {
		return err
	}

	// Rebuild the outputs using the supplied leaves, then check that the
	// required STXO and spender leaves are actually committed. Generating
	// them here would mutate packets and could mask missing or wrong leaves.
	outputs, err := tapsend.CreateOutputCommitments(
		t.Packets, tapsend.WithNoSTXOProofs(),
	)
	if err != nil {
		return fmt.Errorf("invalid output commitments: %w", err)
	}
	if err := tapsend.ValidateAnchorInputs(
		btc, anchorPackets, prunedAssets,
	); err != nil {
		return fmt.Errorf("invalid input commitments: %w", err)
	}
	for _, pkt := range t.Packets {
		for _, out := range pkt.Outputs {
			outputCommitment := outputs[out.AnchorOutputIndex]
			if err := validateTransitionCommitments(
				out.Asset, outputCommitment,
			); err != nil {
				return fmt.Errorf("asset anchor output %d: %w",
					out.AnchorOutputIndex, err)
			}
			script, _, _, err := tapsend.AnchorOutputScript(
				out.AnchorOutputInternalKey,
				out.AnchorOutputTapscriptSibling,
				outputCommitment,
			)
			if err != nil {
				return err
			}
			btcOut := tx.TxOut[out.AnchorOutputIndex]
			if !bytes.Equal(script, btcOut.PkScript) {
				return fmt.Errorf("asset anchor output %d mismatch",
					out.AnchorOutputIndex)
			}
		}
	}

	// Rebuild proofs from the exact transaction instead of trusting cached
	// suffixes. Matching declared anchor scripts alone does not exclude
	// assets or their STXOs from other Bitcoin outputs.
	for pktIdx, pkt := range t.Packets {
		for outIdx := range pkt.Outputs {
			suffix, err := tapsend.CreateProofSuffix(
				tx, btc.Outputs, pkt, outputs, outIdx, t.Packets,
				proof.WithVersion(policy.TransitionVersion),
			)
			if err != nil {
				return fmt.Errorf("cannot construct asset output "+
					"proof %d/%d: %w", pktIdx, outIdx, err)
			}
			if _, err := suffix.VerifyProofs(); err != nil {
				return fmt.Errorf("invalid asset output proof "+
					"%d/%d: %w", pktIdx, outIdx, err)
			}
		}
	}

	return nil
}

// validateAnchorTimeLocks checks that Bitcoin consensus enforces the asset
// timelocks without requiring the anchor to be mature at signing time. The
// packet and Bitcoin inputs have already been structurally validated.
func validateAnchorTimeLocks(pkt *tappsbt.VPacket, tx *wire.MsgTx) error {
	inputs := make(map[wire.OutPoint]*wire.TxIn, len(tx.TxIn))
	var lockTimeEnabled bool
	for _, in := range tx.TxIn {
		inputs[in.PreviousOutPoint] = in
		lockTimeEnabled = lockTimeEnabled ||
			in.Sequence != wire.MaxTxInSequenceNum
	}
	for idx, out := range pkt.Outputs {
		lockTime := out.LockTime
		// The pinned asset VM treats the threshold itself as a height,
		// while Bitcoin treats it as a timestamp. It cannot be safely
		// represented by Bitcoin nLockTime.
		if lockTime > math.MaxUint32 ||
			lockTime == txscript.LockTimeThreshold {

			return fmt.Errorf("output %d has an unsupported absolute "+
				"locktime %d", idx, lockTime)
		}
		if lockTime != 0 {
			assetTime := lockTime > txscript.LockTimeThreshold
			bitcoinTime := tx.LockTime >= txscript.LockTimeThreshold
			if assetTime != bitcoinTime ||
				uint64(tx.LockTime) < lockTime || !lockTimeEnabled {

				return fmt.Errorf("Bitcoin locktime does not enforce "+
					"output %d absolute locktime %d", idx, lockTime)
			}
		}

		if out.RelativeLockTime > math.MaxUint32 {
			return fmt.Errorf("output %d has an unsupported relative "+
				"locktime %d", idx, out.RelativeLockTime)
		}
		relativeLock := uint32(out.RelativeLockTime)
		if relativeLock == 0 ||
			relativeLock&wire.SequenceLockTimeDisabled != 0 {

			continue
		}
		if tx.Version < 2 {
			return fmt.Errorf("Bitcoin version must be at least 2 "+
				"to enforce output %d relative locktime", idx)
		}

		// Split output witnesses may contain ZeroPrevID. Use the real
		// packet inputs so every anchor spending this asset is delayed.
		for _, in := range pkt.Inputs {
			btcInput := inputs[in.PrevID.OutPoint]
			if btcInput == nil {
				return fmt.Errorf("asset input anchor %v is missing",
					in.PrevID.OutPoint)
			}
			sequence := btcInput.Sequence
			if sequence&wire.SequenceLockTimeDisabled != 0 ||
				sequence&wire.SequenceLockTimeIsSeconds !=
					relativeLock&wire.SequenceLockTimeIsSeconds ||
				sequence&wire.SequenceLockTimeMask <
					relativeLock&wire.SequenceLockTimeMask {

				return fmt.Errorf("Bitcoin input %v sequence %d "+
					"does not enforce output %d relative locktime "+
					"%d", in.PrevID.OutPoint, sequence, idx,
					relativeLock)
			}
		}
	}
	return nil
}

// validateTransitionCommitments checks the canonical input tombstones and
// spender locators in the combined Bitcoin output commitment. Leaves can be
// supplied by any virtual output sharing that anchor, but their committed
// contents must match the transition, not just its input-derived leaf keys.
func validateTransitionCommitments(out *asset.Asset,
	outputCommitment *commitment.TapCommitment) error {

	stxos, err := asset.CollectSTXO(out)
	if err != nil {
		return fmt.Errorf("collect STXO transition commitments: %w", err)
	}
	spenders, err := asset.CollectSpenders(out)
	if err != nil {
		return fmt.Errorf("collect spender transition commitments: %w", err)
	}
	for _, leaf := range append(stxos, spenders...) {
		expected := leaf.Copy()
		altCommitment, ok := outputCommitment.Commitment(expected)
		if !ok {
			return fmt.Errorf("missing transition commitment")
		}
		actual, ok := altCommitment.Asset(expected.AssetCommitmentKey())
		if !ok {
			return fmt.Errorf("missing transition commitment leaf")
		}
		expectedLeaf, err := expected.Leaf()
		if err != nil {
			return fmt.Errorf("encode expected transition commitment: %w",
				err)
		}
		actualLeaf, err := actual.Leaf()
		if err != nil {
			return fmt.Errorf("encode actual transition commitment: %w",
				err)
		}
		if expectedLeaf.NodeHash() != actualLeaf.NodeHash() {
			return fmt.Errorf("incorrect transition commitment leaf")
		}
	}
	return nil
}

// anchorAsset returns the on-chain representation of an asset. Non-interactive
// input proofs also carry the split witness, which is not in the anchor leaf.
func anchorAsset(a *asset.Asset) *asset.Asset {
	result := a.Copy()
	trimSplitWitness(result)
	return result
}

// trimSplitWitness removes the split proof absent from an on-chain asset leaf.
func trimSplitWitness(a *asset.Asset) {
	if a.HasSplitCommitmentWitness() &&
		*a.PrevWitnesses[0].PrevID == asset.ZeroPrevID {

		a.PrevWitnesses[0].SplitCommitment = nil
	}
}

// validatePacket checks complete transfers, including conservation across every
// split output. A valid split proof alone does not establish that all parts of
// the split are present in the Bitcoin transaction.
func validatePacket(pkt *tappsbt.VPacket, numOutputs int) error {
	if pkt == nil || len(pkt.Inputs) == 0 || len(pkt.Outputs) == 0 {
		return fmt.Errorf("incomplete virtual packet")
	}
	if !policy.SupportsPacketVersion(pkt.Version) {
		return fmt.Errorf("unsupported virtual packet version %d", pkt.Version)
	}
	prevAssets := make(commitment.InputSet, len(pkt.Inputs))
	var inputAmount, outputAmount uint64
	for _, in := range pkt.Inputs {
		if in == nil || in.Asset() == nil ||
			in.Asset().ScriptKey.PubKey == nil {

			return fmt.Errorf("incomplete virtual input")
		}
		a := in.Asset()
		if in.Proof != nil {
			if err := policy.ValidateTransitionVersion(in.Proof.Version); err != nil {
				return err
			}
		}
		if !policy.SupportsAssetVersion(a.Version) {
			return fmt.Errorf("unsupported input asset version %d", a.Version)
		}
		if in.PrevID.ID != a.ID() || in.PrevID.ScriptKey !=
			asset.ToSerialized(a.ScriptKey.PubKey) {

			return fmt.Errorf("virtual input identity mismatch")
		}
		if _, ok := prevAssets[in.PrevID]; ok {
			return fmt.Errorf("duplicate virtual input")
		}
		if a.Amount > math.MaxInt64-inputAmount {
			return fmt.Errorf("virtual input amount overflow")
		}
		inputAmount += a.Amount
		prevAssets[in.PrevID] = a
	}
	for _, out := range pkt.Outputs {
		if out == nil || out.Asset == nil ||
			out.Asset.ScriptKey.PubKey == nil ||
			out.ScriptKey.PubKey == nil ||
			out.AnchorOutputInternalKey == nil ||
			uint64(out.AnchorOutputIndex) >= uint64(numOutputs) {

			return fmt.Errorf("incomplete virtual output")
		}
		a := out.Asset
		if !policy.SupportsAssetVersion(a.Version) {
			return fmt.Errorf("unsupported output asset version %d", a.Version)
		}
		if a.ID() != pkt.Inputs[0].Asset().ID() ||
			out.Amount != a.Amount || out.AssetVersion != a.Version ||
			!out.ScriptKey.PubKey.IsEqual(a.ScriptKey.PubKey) ||
			out.LockTime != a.LockTime ||
			out.RelativeLockTime != a.RelativeLockTime {

			return fmt.Errorf("virtual output intent mismatch")
		}
		if a.IsBurn() || (a.Amount > 0 &&
			a.ScriptKey.PubKey.IsEqual(asset.NUMSPubKey)) {

			return fmt.Errorf("asset sweep cannot burn assets")
		}
		if a.Amount > math.MaxInt64-outputAmount {
			return fmt.Errorf("virtual output amount overflow")
		}
		outputAmount += a.Amount
	}
	if inputAmount == 0 || inputAmount != outputAmount {
		return fmt.Errorf("asset transfer does not conserve amounts")
	}

	isSplit, err := pkt.HasSplitCommitment()
	if err != nil {
		return err
	}
	root := pkt.Outputs[0].Asset
	var splits []*commitment.SplitAsset
	if isSplit {
		splitRoot, err := pkt.SplitRootOutput()
		if err != nil {
			return err
		}
		root = splitRoot.Asset
		for _, out := range pkt.Outputs {
			a := out.Asset
			if out.Type.IsSplitRoot() {
				a = out.SplitAsset
			}
			if a == nil || a.ScriptKey.PubKey == nil ||
				!a.HasSplitCommitmentWitness() {

				return fmt.Errorf("missing split asset")
			}
			if a.Amount != out.Amount || a.ID() != root.ID() ||
				a.Version != out.AssetVersion ||
				!a.ScriptKey.PubKey.IsEqual(out.ScriptKey.PubKey) {

				return fmt.Errorf("split output intent mismatch")
			}
			splitRoot := &a.PrevWitnesses[0].SplitCommitment.RootAsset
			if !splitRoot.DeepEqual(root) {
				return fmt.Errorf("split root asset mismatch")
			}
			splits = append(splits, &commitment.SplitAsset{
				Asset: *a, OutputIndex: out.AnchorOutputIndex,
			})
		}
	} else if len(pkt.Outputs) != 1 {
		return fmt.Errorf("non-split transfer requires one output")
	}

	// The anchor may be prepared before its Bitcoin CSV lock matures. This
	// validates asset conservation and witnesses, not chain maturity.
	engine, err := vm.New(
		root, splits, prevAssets, vm.WithSkipTimeLockValidation(),
	)
	if err != nil {
		return err
	}
	return engine.Execute()
}
