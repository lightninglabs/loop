// Package tapsdk connects Loop asset contracts and node services to the
// Taproot Assets SDK. Swap policy remains owned by Loop's swapkit package.
package tapsdk

import (
	"bytes"

	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

// AnchorPlan converts a Loop Bitcoin contract into an SDK output plan.
func AnchorPlan(plan swapkit.AnchorPlan) sdk.CustomAnchorOutputPlan {
	leaves := make([]sdk.TapLeaf, len(plan.Leaves))
	for i := range plan.Leaves {
		leaves[i] = sdk.TapLeaf{Script: bytes.Clone(plan.Leaves[i])}
	}
	return sdk.CustomAnchorOutputPlan{
		InternalKey: sdk.InternalKey{
			PubKey: sdk.PubKey(plan.InternalKey),
		},
		Tapscript: sdk.CustomAnchorTapscriptPlan{TapLeaves: leaves},
	}
}

// OPTrueOutput binds an SDK asset allocation to a Loop Bitcoin contract.
// The SDK builder validates the complete output and creates its asset witness.
func OPTrueOutput(output sdk.CustomAssetOutput, key sdk.KeyDescriptor,
	anchor swapkit.AnchorPlan) sdk.CustomAssetOutput {

	output.Script = sdk.CustomAssetScriptPlan{
		Mode:   sdk.CustomAssetScriptOPTrue,
		OPTrue: &sdk.CustomAssetOPTrueScriptPlan{InternalKey: key},
	}
	output.Anchor = AnchorPlan(anchor)
	return output
}

// CooperativeSigningPlan selects a contract's sorted aggregate key path.
// Nonce exchange remains external; sessionContext must not contain secrets.
func CooperativeSigningPlan(participants [][33]byte, inputIndex uint32,
	sessionContext []byte) sdk.CustomAnchorInputSigningPlan {

	keys := make([]sdk.PubKey, len(participants))
	for i := range participants {
		keys[i] = sdk.PubKey(participants[i])
	}
	return sdk.CustomAnchorInputSigningPlan{
		InputIndex: inputIndex,
		MuSig2: &sdk.CustomAnchorMuSig2SigningPlan{
			Participants:   keys,
			SessionContext: bytes.Clone(sessionContext),
		},
	}
}

// ScriptSigningPlan selects a contract's exact Bitcoin leaf for SDK signing.
// The anchor PSBT must also include its leaf script and control block.
func ScriptSigningPlan(spend *swapkit.SwapAnchorSpendInfo,
	inputIndex uint32) sdk.CustomAnchorInputSigningPlan {

	leafHash := txscript.NewBaseTapLeaf(spend.LeafScript).TapHash()
	return sdk.CustomAnchorInputSigningPlan{
		InputIndex: inputIndex,
		ScriptPath: &sdk.CustomAnchorScriptPathSigningPlan{
			LeafHash: sdk.Hash(leafHash),
			RequiredSigners: []sdk.XOnlyPubKey{
				sdk.XOnlyPubKey(spend.RequiredSigner),
			},
		},
	}
}
