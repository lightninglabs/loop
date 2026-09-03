package assets

import (
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightninglabs/taproot-assets/vm"
)

// CreateOpTrueSweepVpkt creates a virtual packet that spends proof-bound
// OP_TRUE assets to the given address.
func CreateOpTrueSweepVpkt(ctx context.Context, proofs []*proof.Proof,
	addr *address.Tap) (*tappsbt.VPacket, error) {

	if len(proofs) == 0 {
		return nil, fmt.Errorf("at least one asset proof is required")
	}
	if addr == nil {
		return nil, fmt.Errorf("sweep address is required")
	}
	if addr.ChainParams == nil || addr.ChainParams.Params == nil ||
		addr.ChainParams.TapHRP == "" {

		return nil, fmt.Errorf("sweep address parameters are incomplete")
	}
	if addr.Amount == 0 {
		return nil, fmt.Errorf("sweep address amount must be positive")
	}
	if addr.AssetID == asset.ZeroID {
		return nil, fmt.Errorf("group sweep addresses are unsupported")
	}
	if address.IsUnknownVersion(addr.Version) {
		return nil, fmt.Errorf("unsupported sweep address version")
	}
	if addr.Version >= address.V2 {
		return nil, fmt.Errorf("version 2 sweep addresses are unsupported")
	}
	if addr.AssetVersion != asset.V0 && addr.AssetVersion != asset.V1 {
		return nil, fmt.Errorf("unsupported sweep asset version")
	}
	opTrueScriptKey, _, _, controlBlock, err := htlc.CreateOpTrueLeaf()
	if err != nil {
		return nil, err
	}
	opTrueScriptKey = asset.NewScriptKey(opTrueScriptKey.PubKey)

	var (
		total         uint64
		seenOutpoints = make(map[wire.OutPoint]struct{}, len(proofs))
	)
	for idx, assetProof := range proofs {
		if assetProof == nil {
			return nil, fmt.Errorf("asset proof %d is nil", idx)
		}
		if _, err := assetProof.VerifyProofs(); err != nil {
			return nil, fmt.Errorf("invalid asset proof %d: %w", idx, err)
		}
		proofID := assetProof.Asset.Genesis.ID()
		if proofID != addr.AssetID {
			return nil, fmt.Errorf(
				"asset proof %d does not match sweep address", idx,
			)
		}
		proofScriptKey := assetProof.Asset.ScriptKey.PubKey
		if proofScriptKey == nil ||
			!proofScriptKey.IsEqual(opTrueScriptKey.PubKey) {

			return nil, fmt.Errorf(
				"asset proof %d is not an OP_TRUE asset", idx,
			)
		}
		if math.MaxUint64-total < assetProof.Asset.Amount {
			return nil, fmt.Errorf("asset proof amount overflow")
		}
		outpoint := assetProof.OutPoint()
		if _, ok := seenOutpoints[outpoint]; ok {
			return nil, fmt.Errorf(
				"asset proof %d duplicates an input outpoint", idx,
			)
		}
		seenOutpoints[outpoint] = struct{}{}
		total += assetProof.Asset.Amount
	}
	if total != addr.Amount {
		return nil, fmt.Errorf("total proof amount does not match address")
	}

	sweepVpkt, err := tappsbt.FromProofs(
		proofs, addr.ChainParams, tappsbt.V1,
	)
	if err != nil {
		return nil, err
	}
	if len(sweepVpkt.Inputs) != len(proofs) {
		return nil, fmt.Errorf("proof inputs were not preserved")
	}
	for idx, input := range sweepVpkt.Inputs {
		if input == nil || input.Anchor.InternalKey == nil {
			return nil, fmt.Errorf("asset input %d is incomplete", idx)
		}
		inputKey := input.Anchor.InternalKey
		input.Anchor.Bip32Derivation = []*psbt.Bip32Derivation{
			{PubKey: inputKey.SerializeCompressed()},
		}
		input.Anchor.TrBip32Derivation =
			[]*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						inputKey,
					),
				},
			}
	}

	destinationScriptKey, err := addr.ScriptKeyForAssetID(addr.AssetID)
	if err != nil {
		return nil, fmt.Errorf("invalid sweep script key: %w", err)
	}
	sweepVpkt.Outputs = append(sweepVpkt.Outputs, &tappsbt.VOutput{
		AssetVersion:                 addr.AssetVersion,
		Amount:                       addr.Amount,
		Interactive:                  true,
		AnchorOutputIndex:            0,
		ScriptKey:                    asset.NewScriptKey(destinationScriptKey),
		AnchorOutputInternalKey:      &addr.InternalKey,
		AnchorOutputTapscriptSibling: addr.TapscriptSibling,
		ProofDeliveryAddress:         &addr.ProofCourierAddr,
	})
	if err := tapsend.PrepareOutputAssets(ctx, sweepVpkt); err != nil {
		return nil, err
	}

	controlBlockBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, err
	}
	opTrueScript, err := htlc.GetOpTrueScript()
	if err != nil {
		return nil, err
	}

	if len(sweepVpkt.Outputs) != 1 || sweepVpkt.Outputs[0] == nil ||
		sweepVpkt.Outputs[0].Asset == nil {

		return nil, fmt.Errorf("prepared asset output is incomplete")
	}
	prevWitnesses, err := sweepVpkt.Outputs[0].PrevWitnesses()
	if err != nil || len(prevWitnesses) != len(sweepVpkt.Inputs) {
		return nil, fmt.Errorf("prepared asset witnesses are incomplete")
	}
	for idx := range prevWitnesses {
		prevWitnesses[idx].TxWitness = wire.TxWitness{
			append([]byte(nil), opTrueScript...),
			append([]byte(nil), controlBlockBytes...),
		}
	}

	prevAssets := make(commitment.InputSet, len(sweepVpkt.Inputs))
	for _, input := range sweepVpkt.Inputs {
		prevAssets[input.PrevID] = input.Asset()
	}
	if err := vm.ValidateWitnesses(
		sweepVpkt.Outputs[0].Asset, nil, prevAssets,
	); err != nil {
		return nil, fmt.Errorf("invalid OP_TRUE asset witnesses: %w", err)
	}

	return sweepVpkt, nil
}
