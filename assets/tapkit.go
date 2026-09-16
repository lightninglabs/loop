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
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
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
	opTrueScriptKey, _, _, controlBlock, err := htlc.CreateOpTrueLeaf()
	if err != nil {
		return nil, err
	}
	opTrueScriptKey = asset.NewScriptKey(opTrueScriptKey.PubKey)

	total := uint64(0)
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
		total += assetProof.Asset.Amount
	}
	if total != addr.Amount {
		return nil, fmt.Errorf("total proof amount does not match address")
	}

	// An address always describes a non-interactive transfer. Let the
	// Taproot Assets address constructor choose both the virtual packet
	// version and the split-root/recipient output layout so these semantics
	// stay aligned with the address version.
	addressVpkt, err := tappsbt.FromAddresses(
		[]*address.Tap{addr}, 1,
	)
	if err != nil {
		return nil, err
	}

	sweepVpkt, err := tappsbt.FromProofs(
		proofs, addr.ChainParams, addressVpkt.Version,
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
	recipientOutput, err := addressVpkt.FirstNonSplitRootOutput()
	if err != nil {
		return nil, err
	}
	recipientOutput.ScriptKey = asset.NewScriptKey(destinationScriptKey)
	sweepVpkt.Outputs = addressVpkt.Outputs
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

	for outputIdx, output := range sweepVpkt.Outputs {
		if output == nil || output.Asset == nil {
			return nil, fmt.Errorf(
				"prepared asset output %d is incomplete", outputIdx,
			)
		}

		outputAssets := []*asset.Asset{output.Asset}
		if output.SplitAsset != nil {
			outputAssets = append(outputAssets, output.SplitAsset)
		}
		for _, outputAsset := range outputAssets {
			prevWitnesses := outputAsset.Witnesses()
			if len(prevWitnesses) != len(sweepVpkt.Inputs) {
				return nil, fmt.Errorf(
					"prepared asset output %d witnesses are "+
						"incomplete", outputIdx,
				)
			}
			for idx := range prevWitnesses {
				prevWitnesses[idx].TxWitness = wire.TxWitness{
					append([]byte(nil), opTrueScript...),
					append([]byte(nil), controlBlockBytes...),
				}
			}
		}
	}

	return sweepVpkt, nil
}
