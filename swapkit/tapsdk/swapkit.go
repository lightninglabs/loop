// Package tapsdk adapts Loop asset swap contracts to the generic Taproot
// Assets SDK custom-anchor builder. Swap policy remains owned by Loop.
package tapsdk

import (
	"bytes"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

// SwapHTLCPolicy identifies the immutable Bitcoin contract of an asset HTLC.
type SwapHTLCPolicy = swapkit.SwapHTLCPolicy

const (
	// SwapHTLCUnspecified forces callers to select a contract explicitly.
	SwapHTLCUnspecified = swapkit.SwapHTLCUnspecified

	// SwapHTLCLegacyCSV preserves the original block-based CSV contract.
	SwapHTLCLegacyCSV = swapkit.SwapHTLCLegacyCSV

	// SwapHTLCStaticLoopInV3 uses Loop's V3 CLTV Bitcoin HTLC contract.
	SwapHTLCStaticLoopInV3 = swapkit.SwapHTLCStaticLoopInV3
)

// SwapKitParams specifies immutable Bitcoin HTLC keys, hash, and expiry.
type SwapKitParams = swapkit.SwapKitParams

// SwapKit combines the shared Bitcoin contract with SDK asset output plans.
// The underlying swapkit module can also be used by applications with an older
// wallet dependency graph without importing tapd or lnd implementations.
type SwapKit struct {
	contract *swapkit.SwapKit
}

// NewSwapKit validates and constructs an asset HTLC policy.
func NewSwapKit(params SwapKitParams) (*SwapKit, error) {
	contract, err := swapkit.NewSwapKit(params)
	if err != nil {
		return nil, err
	}
	return &SwapKit{contract: contract}, nil
}

// Params returns a value copy of the immutable contract parameters.
func (s *SwapKit) Params() SwapKitParams {
	return s.contract.Params()
}

// AnchorPlan returns the cooperative internal key and Bitcoin HTLC leaves.
func (s *SwapKit) AnchorPlan() sdk.CustomAnchorOutputPlan {
	return swapKitAnchorPlan(s.contract.AnchorPlan())
}

// Output creates an OP_TRUE asset output with this Bitcoin HTLC policy.
func (s *SwapKit) Output(params SwapOutputParams) (
	sdk.CustomAssetOutput, error) {

	return newSwapOutput(params, s.AnchorPlan())
}

// CooperativeSigningPlan selects the sorted aggregate key path. Nonce exchange
// remains external; sessionContext must never contain secret nonces.
func (s *SwapKit) CooperativeSigningPlan(inputIndex uint32,
	sessionContext []byte) sdk.CustomAnchorInputSigningPlan {

	return swapKitSigningPlan(
		s.contract.CooperativeParticipants(), inputIndex,
		sessionContext,
	)
}

// SuccessSpend constructs the receiver's Bitcoin script path from the verified
// asset root. The spending transaction needs version >= 2 and sequence >= 1.
func (s *SwapKit) SuccessSpend(assetRoot sdk.Hash) (
	*SwapAnchorSpendInfo, error) {

	return wrapSwapSpend(s.contract.SuccessSpend(assetRoot))
}

// TimeoutSpend constructs the sender's Bitcoin refund path. Legacy policy uses
// CSV; V3 uses nLockTime >= Expiry with a non-final input sequence.
func (s *SwapKit) TimeoutSpend(assetRoot sdk.Hash) (
	*SwapAnchorSpendInfo, error) {

	return wrapSwapSpend(s.contract.TimeoutSpend(assetRoot))
}

// SuccessWitness constructs a complete Bitcoin witness and checks the preimage.
// ApplyScriptPathWitness accepts only its unlocking prefix, omitting the final
// script and control block which the SDK obtains from the committed package.
func (s *SwapKit) SuccessWitness(assetRoot sdk.Hash, signature []byte,
	preimage [32]byte) ([][]byte, error) {

	return s.contract.SuccessWitness(assetRoot, signature, preimage)
}

// TimeoutWitness constructs a complete Bitcoin refund witness.
// ApplyScriptPathWitness accepts only the signature, omitting the final script
// and control block which the SDK obtains from the committed package.
func (s *SwapKit) TimeoutWitness(assetRoot sdk.Hash,
	signature []byte) ([][]byte, error) {

	return s.contract.TimeoutWitness(assetRoot, signature)
}

// DepositKitParams specifies a cooperative deposit with a client CSV refund.
type DepositKitParams = swapkit.DepositKitParams

// DepositKit combines the shared deposit contract with SDK asset output plans.
type DepositKit struct {
	contract *swapkit.DepositKit
}

// NewDepositKit validates and constructs a static asset deposit policy.
func NewDepositKit(params DepositKitParams) (*DepositKit, error) {
	contract, err := swapkit.NewDepositKit(params)
	if err != nil {
		return nil, err
	}
	return &DepositKit{contract: contract}, nil
}

// Params returns a value copy of the immutable deposit parameters.
func (d *DepositKit) Params() DepositKitParams {
	return d.contract.Params()
}

// AnchorPlan returns the cooperative internal key and Bitcoin refund leaf.
func (d *DepositKit) AnchorPlan() sdk.CustomAnchorOutputPlan {
	return swapKitAnchorPlan(d.contract.AnchorPlan())
}

// Output creates an OP_TRUE asset output with this Bitcoin deposit policy.
func (d *DepositKit) Output(params SwapOutputParams) (
	sdk.CustomAssetOutput, error) {

	return newSwapOutput(params, d.AnchorPlan())
}

// CooperativeSigningPlan selects the deposit's sorted aggregate key path.
func (d *DepositKit) CooperativeSigningPlan(inputIndex uint32,
	sessionContext []byte) sdk.CustomAnchorInputSigningPlan {

	return swapKitSigningPlan(
		d.contract.CooperativeParticipants(), inputIndex,
		sessionContext,
	)
}

// TimeoutSpend constructs the client refund from the verified asset root.
func (d *DepositKit) TimeoutSpend(assetRoot sdk.Hash) (
	*SwapAnchorSpendInfo, error) {

	return wrapSwapSpend(d.contract.TimeoutSpend(assetRoot))
}

// TimeoutWitness constructs a complete Bitcoin refund witness.
// ApplyScriptPathWitness accepts only its signature unlocking element.
func (d *DepositKit) TimeoutWitness(assetRoot sdk.Hash,
	signature []byte) ([][]byte, error) {

	return d.contract.TimeoutWitness(assetRoot, signature)
}

// SwapOutputParams selects an asset allocation without changing its Bitcoin
// policy. Amount is in native asset units; AnchorValueSat is independently in
// satoshis. Fungible assets should use a group-key sdk.AssetRef.
type SwapOutputParams struct {
	ID                string
	AssetRef          sdk.AssetRef
	Amount            uint64
	AnchorOutputIndex uint32
	AnchorValueSat    uint64

	// AssetInternalKey must be unique per output. Persist the builder's
	// OPTrueSpend data, since the witness cannot be reused across outputs.
	AssetInternalKey sdk.KeyDescriptor
	ProofDelivery    sdk.CustomAssetProofDelivery
}

// SwapAnchorSpendInfo contains Bitcoin script-path material in SDK types.
// Compare PkScript with the authenticated prevout before signing. This does
// not prove issuance, provenance, amount, confirmation, or timelock maturity.
type SwapAnchorSpendInfo struct {
	InternalKey       sdk.PubKey
	TaprootMerkleRoot sdk.Hash
	PkScript          []byte
	LeafScript        []byte
	ControlBlock      []byte
	RequiredSigner    sdk.XOnlyPubKey
	Sequence          uint32
	LockTime          uint32
}

// SigningPlan selects this exact Bitcoin leaf for the custom-anchor signer.
// The anchor PSBT must also include LeafScript and ControlBlock on its input.
func (s *SwapAnchorSpendInfo) SigningPlan(
	inputIndex uint32) sdk.CustomAnchorInputSigningPlan {

	leafHash := txscript.NewBaseTapLeaf(s.LeafScript).TapHash()
	return sdk.CustomAnchorInputSigningPlan{
		InputIndex: inputIndex,
		ScriptPath: &sdk.CustomAnchorScriptPathSigningPlan{
			LeafHash:        sdk.Hash(leafHash),
			RequiredSigners: []sdk.XOnlyPubKey{s.RequiredSigner},
		},
	}
}

func newSwapOutput(params SwapOutputParams,
	anchor sdk.CustomAnchorOutputPlan) (sdk.CustomAssetOutput, error) {

	// Both values ultimately enter signed transaction fields. Reject values
	// the underlying virtual transaction or Bitcoin encoding cannot carry.
	if params.Amount > math.MaxInt64 ||
		params.AnchorValueSat > math.MaxInt64 {

		return sdk.CustomAssetOutput{}, fmt.Errorf(
			"amount exceeds maximum",
		)
	}
	output := sdk.CustomAssetOutput{
		ID: params.ID, AssetRef: params.AssetRef, Amount: params.Amount,
		AnchorOutputIndex: params.AnchorOutputIndex,
		AnchorValueSat:    params.AnchorValueSat,
		Script: sdk.CustomAssetScriptPlan{
			Mode: sdk.CustomAssetScriptOPTrue,
			OPTrue: &sdk.CustomAssetOPTrueScriptPlan{
				InternalKey: params.AssetInternalKey,
			},
		},
		Anchor: anchor, ProofDelivery: params.ProofDelivery,
	}
	if err := output.Validate(); err != nil {
		return sdk.CustomAssetOutput{}, err
	}
	output.ProofDelivery.OpaqueMetadata = bytes.Clone(
		output.ProofDelivery.OpaqueMetadata,
	)
	return output, nil
}

func swapKitAnchorPlan(plan swapkit.AnchorPlan) sdk.CustomAnchorOutputPlan {
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

func swapKitSigningPlan(participants [][33]byte, inputIndex uint32,
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

func wrapSwapSpend(spend *swapkit.SwapAnchorSpendInfo,
	err error) (*SwapAnchorSpendInfo, error) {

	if err != nil {
		return nil, err
	}
	return &SwapAnchorSpendInfo{
		InternalKey:       sdk.PubKey(spend.InternalKey),
		TaprootMerkleRoot: sdk.Hash(spend.TaprootMerkleRoot),
		PkScript:          bytes.Clone(spend.PkScript),
		LeafScript:        bytes.Clone(spend.LeafScript),
		ControlBlock:      bytes.Clone(spend.ControlBlock),
		RequiredSigner:    sdk.XOnlyPubKey(spend.RequiredSigner),
		Sequence:          spend.Sequence, LockTime: spend.LockTime,
	}, nil
}
