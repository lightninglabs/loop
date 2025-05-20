package assets

import (
	"context"
	"fmt"

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

// CreateOpTrueSweepVpkt creates a VPacket that sweeps the outputs associated
// with the passed in proofs, given that their TAP script is a simple OP_TRUE.
func CreateOpTrueSweepVpkt(ctx context.Context, proofs []*proof.Proof,
	addr *address.Tap, chainParams *address.ChainParams) (
	*tappsbt.VPacket, error) {

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

	// Sanity check that the amount that we're attempting to sweep matches
	// the address amount.
	if total != addr.Amount {
		return nil, fmt.Errorf("total amount of proofs does not " +
			"match the amount of the address")
	}

	sweepVpkt.Outputs = append(sweepVpkt.Outputs, &tappsbt.VOutput{
		AssetVersion:      addr.AssetVersion,
		Amount:            addr.Amount,
		Interactive:       true,
		AnchorOutputIndex: 0,
		ScriptKey: asset.NewScriptKey(
			&addr.ScriptKey,
		),
		AnchorOutputInternalKey:      &addr.InternalKey,
		AnchorOutputTapscriptSibling: addr.TapscriptSibling,
		ProofDeliveryAddress:         &addr.ProofCourierAddr,
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

	firstPrevWitness := &sweepVpkt.Outputs[0].Asset.PrevWitnesses[0]

	if sweepVpkt.Outputs[0].Asset.HasSplitCommitmentWitness() {
		rootAsset := firstPrevWitness.SplitCommitment.RootAsset
		firstPrevWitness = &rootAsset.PrevWitnesses[0]
	}

	firstPrevWitness.TxWitness = witness

	return sweepVpkt, nil
}
