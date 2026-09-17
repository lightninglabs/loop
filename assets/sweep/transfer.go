// Package sweep validates asset transfers before signing their Bitcoin anchor.
package sweep

import (
	"bytes"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightninglabs/taproot-assets/vm"
)

// Transfer is the caller-approved asset transfer to anchor. The caller must
// construct or check the destinations, amounts and anchor keys independently
// of an untrusted Bitcoin PSBT. All active, change and passive assets must be
// included, with their virtual witnesses complete. Validation does not establish
// ownership of destination keys, proof provenance or chain confirmation status.
type Transfer struct {
	Packets []*tappsbt.VPacket

	// PrunedAssets contains only existing tombstones and provable burns
	// omitted from the virtual packets, keyed by their input anchor outpoint.
	PrunedAssets map[wire.OutPoint][]*asset.Asset
}

// Validate binds the proof-selected asset and the complete input commitments to
// valid virtual transitions, then binds every resulting asset commitment to the
// exact Bitcoin outputs being signed. Neither the PSBT nor Transfer is mutated.
func (t *Transfer) Validate(p *proof.Proof, btc *psbt.Packet) error {
	if t == nil || len(t.Packets) == 0 {
		return fmt.Errorf("asset transfer packets are required")
	}
	if p == nil || btc == nil || btc.UnsignedTx == nil {
		return fmt.Errorf("asset proof and Bitcoin PSBT are required")
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

	var found bool
	anchors := make(map[wire.OutPoint]bool)
	anchorPackets := make([]*tappsbt.VPacket, len(t.Packets))
	proofAsset := anchorAsset(&p.Asset)
	for idx, pkt := range t.Packets {
		if err := validatePacket(pkt, len(tx.TxOut)); err != nil {
			return fmt.Errorf("asset transfer %d: %w", idx, err)
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

	// Rebuild the outputs from the supplied complete packets. STXO leaves
	// are already part of AltLeaves on anchored packets; adding them again
	// would both mutate the caller's packets and duplicate those leaves.
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
			script, _, _, err := tapsend.AnchorOutputScript(
				out.AnchorOutputInternalKey,
				out.AnchorOutputTapscriptSibling,
				outputs[out.AnchorOutputIndex],
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

	return nil
}

// anchorAsset returns the on-chain representation of an asset. Non-interactive
// input proofs also carry the split witness, which is not in the anchor leaf.
func anchorAsset(a *asset.Asset) *asset.Asset {
	result := a.Copy()
	trimSplitWitness(result)
	return result
}

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
	prevAssets := make(commitment.InputSet, len(pkt.Inputs))
	var inputAmount, outputAmount uint64
	for _, in := range pkt.Inputs {
		if in == nil || in.Asset() == nil ||
			in.Asset().ScriptKey.PubKey == nil {

			return fmt.Errorf("incomplete virtual input")
		}
		a := in.Asset()
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
