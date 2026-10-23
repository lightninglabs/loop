package tapsdk

import (
	"bytes"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/asset"
)

// LegacyOPTrueDescriptor returns the unchanged NUMS/single-OP_TRUE asset tree.
// Keys are x-only at the asset boundary. The control block retains the actual
// tweaked output parity, and every returned byte slice is independently owned.
func LegacyOPTrueDescriptor() (sdk.ScriptKey,
	*sdk.CustomAssetOPTrueSpendInfo, error) {

	internal := *asset.NUMSPubKey
	leaf := txscript.NewBaseTapLeaf([]byte{txscript.OP_TRUE})
	tree := txscript.AssembleTaprootScriptTree(leaf)
	root := tree.RootNode.TapHash()
	output := txscript.ComputeTaprootOutputKey(&internal, root[:])
	even, err := schnorr.ParsePubKey(schnorr.SerializePubKey(output))
	if err != nil {
		return sdk.ScriptKey{}, nil, err
	}
	key, err := sdk.ParsePubKey(even.SerializeCompressed())
	if err != nil {
		return sdk.ScriptKey{}, nil, err
	}
	raw, err := sdk.ParsePubKey(internal.SerializeCompressed())
	if err != nil {
		return sdk.ScriptKey{}, nil, err
	}
	control := tree.LeafMerkleProofs[0].ToControlBlock(&internal)
	encoded, err := control.ToBytes()
	if err != nil {
		return sdk.ScriptKey{}, nil, err
	}
	spend := &sdk.CustomAssetOPTrueSpendInfo{
		LeafScript:   bytes.Clone(leaf.Script),
		LeafVersion:  uint8(leaf.LeafVersion),
		ControlBlock: encoded, InternalKey: raw,
	}
	if err := spend.Validate(key); err != nil {
		return sdk.ScriptKey{}, nil, err
	}
	return sdk.ScriptKey{PubKey: key,
		KeyDesc:  sdk.KeyDescriptor{RawKeyBytes: raw},
		TapTweak: bytes.Clone(root[:])}, spend, nil
}

// LegacyOPTrueOutput binds an allocation to the existing asset script policy.
// Native SDK External mode preserves the exact key and derivation. No wallet
// key or asset-ID leaf is added; selecting SDKOPTrueOutput is a separate choice.
func LegacyOPTrueOutput(output sdk.CustomAssetOutput,
	anchor swapkit.AnchorPlan) (sdk.CustomAssetOutput, error) {

	key, _, err := LegacyOPTrueDescriptor()
	if err != nil {
		return sdk.CustomAssetOutput{}, err
	}
	output.Script = sdk.CustomAssetScriptPlan{
		Mode:     sdk.CustomAssetScriptExternal,
		External: &sdk.CustomAssetExternalScriptPlan{ScriptKey: key},
	}
	output.Anchor = AnchorPlan(anchor)
	return output, nil
}

// SDKOPTrueOutput explicitly selects the SDK's supplied-key/asset-ID tree.
// Existing funded outputs always retain their saved key and witness instead.
func SDKOPTrueOutput(output sdk.CustomAssetOutput, key sdk.KeyDescriptor,
	anchor swapkit.AnchorPlan) sdk.CustomAssetOutput {

	return OPTrueOutput(output, key, anchor)
}

// OPTrueSpendInfo validates a saved SDK descriptor or reconstructs only the
// exact legacy External tree. An ordinary wallet External key is never treated
// as OP_TRUE, and native sealed package summaries are never modified.
func OPTrueSpendInfo(output sdk.CustomAnchorAssetOutputSummary) (
	*sdk.CustomAssetOPTrueSpendInfo, error) {

	if output.OPTrueSpend != nil {
		if err := output.OPTrueSpend.Validate(output.ScriptKey); err != nil {
			return nil, err
		}
		return output.OPTrueSpend.Clone(), nil
	}
	if output.ScriptMode != sdk.CustomAssetScriptExternal {
		return nil, swapkit.ErrInvalid
	}
	key, spend, err := LegacyOPTrueDescriptor()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(key.PubKey[1:], output.ScriptKey[1:]) {
		return nil, swapkit.ErrInvalid
	}
	if err := spend.Validate(output.ScriptKey); err != nil {
		return nil, err
	}
	return spend, nil
}

// AssetInput supplies a proof-selected OP_TRUE asset to the native SDK. This
// binds the exact issuance, quantity, outpoint and script key before generic
// SDK proof verification; the FSM still checks the current chain and contract.
func AssetInput(output sdk.CustomAnchorAssetOutputSummary,
	raw []byte) (sdk.CustomAssetInput, error) {

	spend, err := OPTrueSpendInfo(output)
	if err != nil {
		return sdk.CustomAssetInput{}, err
	}
	p, err := LastProof(raw)
	if err != nil {
		return sdk.CustomAssetInput{}, err
	}
	if sdk.AssetID(p.Asset.ID()) != output.IssuanceID ||
		p.Asset.Amount != output.Amount ||
		p.OutPoint() != SDKPoint(output.AnchorOutpoint) ||
		!bytes.Equal(p.Asset.ScriptKey.PubKey.SerializeCompressed()[1:],
			output.ScriptKey[1:]) {

		return sdk.CustomAssetInput{}, swapkit.ErrInvalid
	}
	return sdk.CustomAssetInput{
		ID: output.LogicalOutputID, AssetRef: output.AssetRef,
		Amount: output.Amount, ProofFile: bytes.Clone(raw),
		Witness: sdk.CustomAssetWitnessPlan{
			Mode:  sdk.CustomAssetWitnessCallerProvided,
			Stack: spend.WitnessStack(),
		},
	}, nil
}
