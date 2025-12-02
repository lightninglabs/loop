package assets

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
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
)

// GenTaprootAssetRootFromProof generates the taproot asset root from the proof
// of the swap.
func GenTaprootAssetRootFromProof(proof *proof.Proof) ([]byte, error) {
	assetCopy := proof.Asset.CopySpendTemplate()

	version := commitment.TapCommitmentV2
	assetCommitment, err := commitment.FromAssets(&version, assetCopy)
	if err != nil {
		return nil, err
	}

	assetCommitment, err = commitment.TrimSplitWitnesses(
		&version, assetCommitment,
	)
	if err != nil {
		return nil, err
	}

	taprootAssetRoot := assetCommitment.TapscriptRoot(nil)

	return taprootAssetRoot[:], nil
}

// CreateOpTrueSweepVpkt creates a VPacket that interactively sweeps the outputs
// associated with the passed in proofs, given that their TAP script is a simple
// OP_TRUE.
func CreateOpTrueSweepVpkt(ctx context.Context, proofs []*proof.Proof,
	sweepScriptKey asset.ScriptKey, sweepInternalKey *btcec.PublicKey,
	tapScriptSibling *commitment.TapscriptPreimage,
	chainParams *address.ChainParams) (*tappsbt.VPacket, error) {

	sweepVpkt, err := tappsbt.FromProofs(proofs, chainParams, tappsbt.V1)
	if err != nil {
		return nil, err
	}

	total := uint64(0)
	for i, proof := range proofs {
		inputKey := proof.InclusionProof.InternalKey

		sweepVpkt.Inputs[i].Anchor.Bip32Derivation =
			[]*psbt.Bip32Derivation{
				{
					PubKey: inputKey.SerializeCompressed(),
				},
			}
		sweepVpkt.Inputs[i].Anchor.TrBip32Derivation =
			[]*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(
						inputKey,
					),
				},
			}

		total += proof.Asset.Amount
	}

	sweepVpkt.Outputs = append(sweepVpkt.Outputs, &tappsbt.VOutput{
		AssetVersion:                 asset.V1,
		Amount:                       total,
		Interactive:                  true,
		AnchorOutputIndex:            0,
		ScriptKey:                    sweepScriptKey,
		AnchorOutputInternalKey:      sweepInternalKey,
		AnchorOutputTapscriptSibling: tapScriptSibling,
		ProofDeliveryAddress:         nil,
	})

	err = tapsend.PrepareOutputAssets(ctx, sweepVpkt)
	if err != nil {
		return nil, err
	}

	_, _, _, controlBlock, err := htlc.CreateOpTrueLeaf()
	if err != nil {
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

	witness := wire.TxWitness{
		opTrueScript,
		controlBlockBytes,
	}

	err = sweepVpkt.Outputs[0].Asset.UpdateTxWitness(0, witness)
	if err != nil {
		return nil, fmt.Errorf("unable to update witness: %w", err)
	}

	return sweepVpkt, nil
}
