package htlc

import (
	"bytes"
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/assets/internal/policy"
	assetsweep "github.com/lightninglabs/loop/assets/sweep"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
)

// SuccessSequence is the relative lock used by both HTLC success policies.
const SuccessSequence uint32 = 1

// Policy identifies immutable asset HTLC contract terms, independently of the
// swap direction. Changed terms require a distinct policy; protocols using
// identical terms explicitly select the same policy.
type Policy uint8

const (
	// PolicyUnknown reserves the zero value so every caller must explicitly
	// choose the contract terms it expects.
	PolicyUnknown Policy = 0

	// AssetHTLCV0 preserves the existing asset deposit HTLC contract for
	// both Asset Loop In and Asset Loop Out. Its value is the historical
	// LegacyDepositV0 policy value, not a Taproot Assets wire version.
	AssetHTLCV0 Policy = 1

	// LegacyDepositV0 is the compatibility name for AssetHTLCV0. Existing
	// drafts can keep selecting it without changing their contract terms.
	LegacyDepositV0 = AssetHTLCV0

	// AssetHTLCCLTVV1 uses an absolute Bitcoin block height for refunds,
	// while retaining the V0 success path, key roles and asset script key.
	// It can be used for either asset swap direction.
	AssetHTLCCLTVV1 Policy = 2
)

// Params contains the validated inputs used to construct a SwapKit. Sender
// and receiver follow the on-chain asset direction: client to server for Loop
// In, and server to client for Loop Out. A party may reuse its own internal key
// as its script key to reproduce the legacy two-key contract.
type Params struct {
	// Internal keys authorize cooperative Bitcoin key-path spends.
	SenderInternalPubKey   *btcec.PublicKey
	ReceiverInternalPubKey *btcec.PublicKey

	// Script keys authorize the sender's refund and receiver's claim.
	SenderScriptKey   *btcec.PublicKey
	ReceiverScriptKey *btcec.PublicKey
	AssetID           asset.ID
	Amount            uint64
	SwapHash          lntypes.Hash

	// CsvExpiry is the relative refund delay for AssetHTLCV0 only.
	CsvExpiry uint32

	// RefundHeight is the absolute Bitcoin refund height for
	// AssetHTLCCLTVV1 only. Timestamp locktimes are not supported.
	RefundHeight  uint32
	AddressParams *address.ChainParams
}

// SwapKit holds the immutable information required to construct and spend an
// on-chain Taproot Asset HTLC. NewSwapKit is the only constructor so a funded
// contract cannot later be changed by mutating a key, hash, amount, or expiry.
type SwapKit struct {
	senderInternalPubKey   *btcec.PublicKey
	receiverInternalPubKey *btcec.PublicKey
	policy                 Policy
	senderScriptKey        *btcec.PublicKey
	receiverScriptKey      *btcec.PublicKey
	assetID                asset.ID
	amount                 uint64
	swapHash               lntypes.Hash
	csvExpiry              uint32
	refundHeight           uint32
	addressParams          address.ChainParams
}

// NewSwapKit returns a defensively validated asset HTLC kit.
func NewSwapKit(contractPolicy Policy, params Params) (*SwapKit, error) {
	addressParams, err := policy.CanonicalAddressParams(
		params.AddressParams,
	)
	if err != nil {
		return nil, err
	}

	senderInternalKey, err := clonePublicKey(
		params.SenderInternalPubKey, "sender internal",
	)
	if err != nil {
		return nil, err
	}
	receiverInternalKey, err := clonePublicKey(
		params.ReceiverInternalPubKey, "receiver internal",
	)
	if err != nil {
		return nil, err
	}
	senderKey, err := clonePublicKey(
		params.SenderScriptKey, "sender script",
	)
	if err != nil {
		return nil, err
	}
	receiverKey, err := clonePublicKey(
		params.ReceiverScriptKey, "receiver script",
	)
	if err != nil {
		return nil, err
	}

	kit := &SwapKit{
		senderInternalPubKey:   senderInternalKey,
		receiverInternalPubKey: receiverInternalKey,
		policy:                 contractPolicy,
		senderScriptKey:        senderKey,
		receiverScriptKey:      receiverKey,
		assetID:                params.AssetID,
		amount:                 params.Amount,
		swapHash:               params.SwapHash,
		csvExpiry:              params.CsvExpiry,
		refundHeight:           params.RefundHeight,
		addressParams:          *addressParams,
	}
	if err := kit.Validate(); err != nil {
		return nil, err
	}

	return kit, nil
}

// clonePublicKey validates a named key role and returns an independent copy.
func clonePublicKey(key *btcec.PublicKey, role string) (*btcec.PublicKey, error) {
	if key == nil {
		return nil, fmt.Errorf("%s public key is required", role)
	}
	keyCopy, err := btcec.ParsePubKey(key.SerializeCompressed())
	if err != nil {
		return nil, fmt.Errorf("invalid %s public key: %w", role, err)
	}

	return keyCopy, nil
}

// Policy returns the immutable contract policy.
func (s *SwapKit) Policy() Policy {
	return s.policy
}

// SenderInternalPubKey returns the sender's cooperative internal public key.
func (s *SwapKit) SenderInternalPubKey() *btcec.PublicKey {
	key, _ := clonePublicKey(s.senderInternalPubKey, "sender internal")

	return key
}

// ReceiverInternalPubKey returns the receiver's cooperative internal public key.
func (s *SwapKit) ReceiverInternalPubKey() *btcec.PublicKey {
	key, _ := clonePublicKey(s.receiverInternalPubKey, "receiver internal")

	return key
}

// SenderScriptKey returns the sender's refund public key.
func (s *SwapKit) SenderScriptKey() *btcec.PublicKey {
	key, _ := btcec.ParsePubKey(s.senderScriptKey.SerializeCompressed())

	return key
}

// ReceiverScriptKey returns the receiver's claim public key.
func (s *SwapKit) ReceiverScriptKey() *btcec.PublicKey {
	key, _ := btcec.ParsePubKey(s.receiverScriptKey.SerializeCompressed())

	return key
}

// AssetID returns the exact asset identifier.
func (s *SwapKit) AssetID() asset.ID {
	return s.assetID
}

// Amount returns the amount in the asset's native unit.
func (s *SwapKit) Amount() uint64 {
	return s.amount
}

// SwapHash returns the payment hash committed to by the success path.
func (s *SwapKit) SwapHash() lntypes.Hash {
	return s.swapHash
}

// CsvExpiry returns the relative refund delay, or zero for a CLTV contract.
func (s *SwapKit) CsvExpiry() uint32 {
	return s.csvExpiry
}

// RefundHeight returns the CLTV refund height, or zero for a CSV contract.
func (s *SwapKit) RefundHeight() uint32 {
	return s.refundHeight
}

// AddressParams returns a copy of the contract's address parameters.
func (s *SwapKit) AddressParams() address.ChainParams {
	return *policy.CloneAddressParams(s.addressParams)
}

// Validate verifies every field required to create an HTLC virtual packet.
func (s *SwapKit) Validate() error {
	if err := s.validateScripts(); err != nil {
		return err
	}
	if s.amount == 0 {
		return fmt.Errorf("asset amount must be positive")
	}
	if s.amount > math.MaxInt64 {
		return fmt.Errorf("asset amount exceeds maximum virtual output value")
	}
	if s.assetID == (asset.ID{}) {
		return fmt.Errorf("asset ID is required")
	}
	if s.swapHash == (lntypes.Hash{}) {
		return fmt.Errorf("swap hash is required")
	}

	return nil
}

// validateScripts checks the immutable policy inputs used to derive the HTLC
// script tree.
func (s *SwapKit) validateScripts() error {
	if s == nil {
		return fmt.Errorf("swap kit is required")
	}
	if s.policy != AssetHTLCV0 && s.policy != AssetHTLCCLTVV1 {
		return fmt.Errorf("unknown asset HTLC policy: %d", s.policy)
	}
	if s.senderScriptKey == nil {
		return fmt.Errorf("sender script public key is required")
	}
	if s.receiverScriptKey == nil {
		return fmt.Errorf("receiver script public key is required")
	}
	if s.senderInternalPubKey == nil || s.receiverInternalPubKey == nil {
		return fmt.Errorf("sender and receiver internal public keys are required")
	}
	senderKeys := []*btcec.PublicKey{
		s.senderInternalPubKey, s.senderScriptKey,
	}
	receiverKeys := []*btcec.PublicKey{
		s.receiverInternalPubKey, s.receiverScriptKey,
	}
	for _, senderKey := range senderKeys {
		for _, receiverKey := range receiverKeys {
			if bytes.Equal(schnorr.SerializePubKey(senderKey),
				schnorr.SerializePubKey(receiverKey)) {

				return fmt.Errorf("sender and receiver keys must " +
					"differ across roles")
			}
		}
	}
	switch s.policy {
	case AssetHTLCV0:
		if s.refundHeight != 0 {
			return fmt.Errorf("CSV policy cannot set refund height")
		}
		if s.csvExpiry <= SuccessSequence {
			return fmt.Errorf("CSV expiry must exceed success sequence")
		}
		if s.csvExpiry > wire.SequenceLockTimeMask {
			return fmt.Errorf("CSV expiry exceeds block-based BIP68 range")
		}

	case AssetHTLCCLTVV1:
		if s.csvExpiry != 0 {
			return fmt.Errorf("CLTV policy cannot set CSV expiry")
		}
		if s.refundHeight == 0 ||
			s.refundHeight >= txscript.LockTimeThreshold {

			return fmt.Errorf("refund height must be positive and below %d",
				uint32(txscript.LockTimeThreshold))
		}
	}

	return nil
}

// GetSuccessScript returns the success path script of the swap HTLC.
func (s *SwapKit) GetSuccessScript() ([]byte, error) {
	if s == nil || s.receiverScriptKey == nil {
		return nil, fmt.Errorf("receiver script public key is required")
	}
	if s.policy != AssetHTLCV0 && s.policy != AssetHTLCCLTVV1 {
		return nil, fmt.Errorf("unknown asset HTLC policy: %d", s.policy)
	}

	return GenSuccessPathScript(s.receiverScriptKey, s.swapHash)
}

// GetTimeoutScript returns the timeout path script of the swap HTLC.
func (s *SwapKit) GetTimeoutScript() ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("swap kit is required")
	}
	switch s.policy {
	case AssetHTLCV0:
		return GenTimeoutPathScript(s.senderScriptKey, int64(s.csvExpiry))

	case AssetHTLCCLTVV1:
		return GenCLTVTimeoutPathScript(
			s.senderScriptKey, int64(s.refundHeight),
		)

	default:
		return nil, fmt.Errorf("unknown asset HTLC policy: %d", s.policy)
	}
}

// GetAggregateKey returns the sorted MuSig2 aggregate key used in the swap
// HTLC from the two internal keys. The key is returned before any Taproot tweak.
func (s *SwapKit) GetAggregateKey() (*btcec.PublicKey, error) {
	if err := s.validateScripts(); err != nil {
		return nil, err
	}

	aggregateKey, err := input.MuSig2CombineKeys(
		input.MuSig2Version100RC2,
		[]*btcec.PublicKey{s.senderInternalPubKey, s.receiverInternalPubKey},
		true,
		&input.MuSig2Tweaks{},
	)
	if err != nil {
		return nil, err
	}

	return aggregateKey.PreTweakedKey, nil
}

// GetTimeoutLeaf returns the timeout leaf of the swap.
func (s *SwapKit) GetTimeoutLeaf() (txscript.TapLeaf, error) {
	timeoutScript, err := s.GetTimeoutScript()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(timeoutScript), nil
}

// GetTimeOutLeaf returns the timeout leaf of the swap. The historical method
// capitalization is retained while callers migrate to GetTimeoutLeaf.
func (s *SwapKit) GetTimeOutLeaf() (txscript.TapLeaf, error) {
	return s.GetTimeoutLeaf()
}

// GetSuccessLeaf returns the success leaf of the swap.
func (s *SwapKit) GetSuccessLeaf() (txscript.TapLeaf, error) {
	successScript, err := s.GetSuccessScript()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(successScript), nil
}

// GetSiblingPreimage returns the branch preimage placed beside the Taproot
// Asset commitment in the Bitcoin output tree.
func (s *SwapKit) GetSiblingPreimage() (
	commitment.TapscriptPreimage, error) {

	timeoutLeaf, err := s.GetTimeoutLeaf()
	if err != nil {
		return commitment.TapscriptPreimage{}, err
	}

	successLeaf, err := s.GetSuccessLeaf()
	if err != nil {
		return commitment.TapscriptPreimage{}, err
	}

	branch := txscript.NewTapBranch(timeoutLeaf, successLeaf)

	return commitment.NewPreimageFromBranch(branch), nil
}

// CreateHtlcVpkt creates a non-interactive version-one HTLC funding template
// matching the predictive V1 contract address. It must pass through
// FundVirtualPsbt before being prepared or committed as a final virtual
// packet. Funding fills the change output or retains a zero-valued NUMS
// tombstone for exact-value sends, so the HTLC remains a predictable split
// leaf in either case. The funder must use the contract's asset and group.
func (s *SwapKit) CreateHtlcVpkt() (*tappsbt.VPacket, error) {
	return s.createHtlcFundingTemplate(false)
}

// CreateInteractiveHtlcVpkt creates the legacy interactive funding template.
// It must pass through FundVirtualPsbt, which fills or removes the zero-valued
// change placeholder. This explicit API preserves the original serialized
// template for existing integrations. Exact-value funding makes the HTLC the
// transfer root and does not match a predictive V1 address; both parties must
// agree to interactive funding and validate the actual anchor proof instead.
func (s *SwapKit) CreateInteractiveHtlcVpkt() (*tappsbt.VPacket, error) {
	return s.createHtlcFundingTemplate(true)
}

// createHtlcFundingTemplate constructs the shared contract layout with uniform
// funding flags on its change and recipient outputs.
func (s *SwapKit) createHtlcFundingTemplate(interactive bool) (
	*tappsbt.VPacket, error) {

	if err := s.Validate(); err != nil {
		return nil, err
	}

	btcInternalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}

	siblingPreimage, err := s.GetSiblingPreimage()
	if err != nil {
		return nil, err
	}

	tapScriptKey, _, _, _, err := CreateOpTrueLeaf()
	if err != nil {
		return nil, err
	}

	pkt := &tappsbt.VPacket{
		Inputs: []*tappsbt.VInput{{
			PrevID: asset.PrevID{ID: s.assetID},
		}},
		Outputs:     make([]*tappsbt.VOutput, 0, 2),
		ChainParams: policy.CloneAddressParams(s.addressParams),
		Version:     policy.PacketVersion,
	}
	pkt.Outputs = append(pkt.Outputs, &tappsbt.VOutput{
		AssetVersion:      policy.AssetVersion,
		Amount:            0,
		Type:              tappsbt.TypeSplitRoot,
		AnchorOutputIndex: 0,
		ScriptKey:         asset.NewScriptKey(asset.NUMSPubKey),
		Interactive:       interactive,
	})
	pkt.Outputs = append(pkt.Outputs, &tappsbt.VOutput{
		AssetVersion:                 policy.AssetVersion,
		Amount:                       s.amount,
		Interactive:                  interactive,
		AnchorOutputIndex:            1,
		ScriptKey:                    asset.NewScriptKey(tapScriptKey.PubKey),
		AnchorOutputInternalKey:      btcInternalKey,
		AnchorOutputTapscriptSibling: &siblingPreimage,
	})

	return pkt, nil
}

// GenTimeoutBtcControlBlock generates the Bitcoin control block for the
// timeout path. The supplied root must be the exact Taproot Asset commitment
// root committed to by the anchor output.
func (s *SwapKit) GenTimeoutBtcControlBlock(taprootAssetRoot []byte) (
	*txscript.ControlBlock, error) {

	return s.genBtcControlBlock(false, taprootAssetRoot)
}

// GenSuccessBtcControlBlock generates the Bitcoin control block for the
// success path.
func (s *SwapKit) GenSuccessBtcControlBlock(taprootAssetRoot []byte) (
	*txscript.ControlBlock, error) {

	return s.genBtcControlBlock(true, taprootAssetRoot)
}

// GetPkScriptFromRoot returns the Bitcoin anchor script for an exact Taproot
// Asset commitment root.
func (s *SwapKit) GetPkScriptFromRoot(taprootAssetRoot []byte) (
	[]byte, error) {

	controlBlock, err := s.GenSuccessBtcControlBlock(taprootAssetRoot)
	if err != nil {
		return nil, err
	}
	successScript, err := s.GetSuccessScript()
	if err != nil {
		return nil, err
	}

	rootHash := controlBlock.RootHash(successScript)
	internalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}
	outputKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash)

	return txscript.PayToTaprootScript(outputKey)
}

// genBtcControlBlock derives a control block for either the success or timeout
// leaf under the proof-derived Taproot Asset root.
func (s *SwapKit) genBtcControlBlock(success bool,
	taprootAssetRoot []byte) (*txscript.ControlBlock, error) {

	if len(taprootAssetRoot) != 32 {
		return nil, fmt.Errorf("Taproot Asset root must be 32 bytes")
	}

	internalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}

	timeoutLeaf, err := s.GetTimeoutLeaf()
	if err != nil {
		return nil, err
	}
	successLeaf, err := s.GetSuccessLeaf()
	if err != nil {
		return nil, err
	}

	spendLeaf := timeoutLeaf
	siblingLeaf := successLeaf
	if success {
		spendLeaf = successLeaf
		siblingLeaf = timeoutLeaf
	}

	siblingHash := siblingLeaf.TapHash()
	inclusionProof := make([]byte, 0, 64)
	inclusionProof = append(inclusionProof, siblingHash[:]...)
	inclusionProof = append(inclusionProof, taprootAssetRoot...)

	controlBlock := &txscript.ControlBlock{
		InternalKey:    internalKey,
		LeafVersion:    txscript.BaseLeafVersion,
		InclusionProof: inclusionProof,
	}

	rootHash := controlBlock.RootHash(spendLeaf.Script)
	tapKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash)
	if tapKey.SerializeCompressed()[0] ==
		secp256k1.PubKeyFormatCompressedOdd {

		controlBlock.OutputKeyYIsOdd = true
	}

	return controlBlock, nil
}

// VerifyCompleteAnchor verifies the proof's commitment proofs and requires the
// proof to describe its complete anchor commitment: the proven asset, the
// proof's alt leaves and nothing else. Signing a later spend requires the
// complete input commitment, so a funder could otherwise co-anchor a leaf it
// does not disclose and block the receiver's claim. Transfers anchored by tapd
// meet this requirement, as tapd copies every alt leaf of an anchor into its
// proofs and anchors passive assets only to outputs its own wallet controls.
// An anchor holding several tranches of a grouped asset does not. The proven
// commitment is returned. The anchor output is not bound to any HTLC
// contract; SwapKit.ValidateProof binds it.
func VerifyCompleteAnchor(assetProof *proof.Proof) (*commitment.TapCommitment,
	error) {

	if assetProof == nil {
		return nil, fmt.Errorf("asset proof is required")
	}
	if err := policy.ValidateTransitionVersion(assetProof.Version); err != nil {
		return nil, err
	}
	if !policy.SupportsAssetVersion(assetProof.Asset.Version) {
		return nil, fmt.Errorf("unsupported input asset version %d",
			assetProof.Asset.Version)
	}

	provenCommitment, err := assetProof.VerifyProofs()
	if err != nil {
		return nil, fmt.Errorf("invalid asset proof: %w", err)
	}
	if err := policy.ValidateCommitmentVersion(provenCommitment.Version); err != nil {
		return nil, err
	}

	// Rebuild the input commitment the way the sweep validator does before
	// signing: the on-chain leaf without its split proof, at version two
	// and then at the legacy versions.
	anchorAsset := assetProof.Asset.Copy()
	if anchorAsset.HasSplitCommitmentWitness() &&
		*anchorAsset.PrevWitnesses[0].PrevID == asset.ZeroPrevID {

		anchorAsset.PrevWitnesses[0].SplitCommitment = nil
	}
	version := policy.CommitmentVersion
	rebuiltCommitment, err := commitment.FromAssets(&version, anchorAsset)
	if err != nil {
		return nil, fmt.Errorf("unable to rebuild asset anchor: %w", err)
	}
	err = rebuiltCommitment.MergeAltLeaves(
		asset.CopyAltLeaves(assetProof.AltLeaves),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid asset proof alt leaves: %w", err)
	}

	provenRoot := provenCommitment.TapscriptRoot(nil)
	if rebuiltCommitment.TapscriptRoot(nil) == provenRoot {
		return provenCommitment, nil
	}
	legacyCommitment, err := rebuiltCommitment.Downgrade()
	if err != nil {
		return nil, fmt.Errorf("unable to rebuild legacy asset anchor: %w",
			err)
	}
	if legacyCommitment.TapscriptRoot(nil) != provenRoot {
		return nil, fmt.Errorf("asset anchor does not match the proven " +
			"asset and alt leaves")
	}

	return provenCommitment, nil
}

// GenTaprootAssetRootFromProof verifies the inclusion, exclusion, and optional
// split-root commitment proofs, and that the proof describes the complete
// anchor commitment, before returning the committed Taproot Asset root. It
// does not verify proof provenance, chain inclusion, confirmation depth, or
// whether the anchor outpoint remains unspent, and it does not bind the anchor
// output to an HTLC contract; SwapKit.ValidateProof does.
func GenTaprootAssetRootFromProof(assetProof *proof.Proof) ([]byte, error) {
	assetCommitment, err := VerifyCompleteAnchor(assetProof)
	if err != nil {
		return nil, err
	}

	taprootAssetRoot := assetCommitment.TapscriptRoot(nil)

	return taprootAssetRoot[:], nil
}

// GetPkScriptFromProof returns the HTLC output script of a proof that
// ValidateProof binds to this contract. The proof is required because an asset
// alone does not identify its anchor commitment version or alt leaves.
func (s *SwapKit) GetPkScriptFromProof(assetProof *proof.Proof) (
	[]byte, error) {

	return s.validateProof(assetProof)
}

// ValidateProof binds a structurally valid proof to this HTLC contract. The
// proven asset must match the swap, and the proof must describe its complete
// anchor commitment, as the receiver could not claim an HTLC that co-anchors
// leaves missing from it. The anchor output must commit to that commitment
// under this contract's aggregate internal key and HTLC scripts, so a funder
// cannot pass off an output that it controls alone as the HTLC. The proof must
// also satisfy the network's Taproot Assets v0.8.5 activation rules for its
// claimed height. It does not verify proof provenance, chain inclusion,
// confirmation depth, or whether the anchor outpoint remains unspent; use
// VerifyProofFile to accept a proof whose history is not yet verified.
func (s *SwapKit) ValidateProof(assetProof *proof.Proof) error {
	_, err := s.validateProof(assetProof)

	return err
}

// validateProof implements ValidateProof and returns the HTLC output script.
func (s *SwapKit) validateProof(assetProof *proof.Proof) ([]byte, error) {
	if assetProof == nil {
		return nil, fmt.Errorf("asset proof is required")
	}
	if err := s.validateAsset(&assetProof.Asset); err != nil {
		return nil, err
	}

	assetCommitment, err := VerifyCompleteAnchor(assetProof)
	if err != nil {
		return nil, err
	}

	// From the activation height, nodes running Taproot Assets v0.8.5
	// cannot verify a transfer that lacks the evidence its rules require,
	// so such an HTLC asset could never be spent with a valid history.
	activationHeight, activated := policy.ActivationHeight(&s.addressParams)
	if activated {
		err := policy.ValidateActivation(assetProof, activationHeight)
		if err != nil {
			return nil, fmt.Errorf("HTLC proof: %w", err)
		}
	}

	// The verified proof reproduces its anchor output from its own internal
	// key and tapscript sibling, so both must be the contract's.
	internalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}
	proofKey := assetProof.InclusionProof.InternalKey
	if proofKey == nil || !bytes.Equal(
		schnorr.SerializePubKey(proofKey),
		schnorr.SerializePubKey(internalKey),
	) {

		return nil, fmt.Errorf("HTLC proof internal key does not match " +
			"the contract")
	}

	sibling, err := s.GetSiblingPreimage()
	if err != nil {
		return nil, err
	}
	expectedSibling, _, err := commitment.MaybeEncodeTapscriptPreimage(
		&sibling,
	)
	if err != nil {
		return nil, err
	}
	commitmentProof := assetProof.InclusionProof.CommitmentProof
	if commitmentProof == nil {
		return nil, fmt.Errorf("HTLC proof commitment proof is required")
	}
	actualSibling, _, err := commitment.MaybeEncodeTapscriptPreimage(
		commitmentProof.TapSiblingPreimage,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid HTLC proof sibling: %w", err)
	}
	if !bytes.Equal(expectedSibling, actualSibling) {
		return nil, fmt.Errorf("HTLC proof tapscript sibling does not " +
			"match the contract")
	}

	taprootAssetRoot := assetCommitment.TapscriptRoot(nil)
	pkScript, err := s.GetPkScriptFromRoot(taprootAssetRoot[:])
	if err != nil {
		return nil, err
	}
	outputIndex := int(assetProof.InclusionProof.OutputIndex)
	if outputIndex >= len(assetProof.AnchorTx.TxOut) ||
		assetProof.AnchorTx.TxOut[outputIndex] == nil {

		return nil, fmt.Errorf("proof anchor output is missing")
	}
	anchorOutput := assetProof.AnchorTx.TxOut[outputIndex]
	if !bytes.Equal(anchorOutput.PkScript, pkScript) {
		return nil, fmt.Errorf("HTLC proof anchor output does not match " +
			"the contract")
	}

	return pkScript, nil
}

// ProofVerifier is the narrow tapd boundary needed to verify a complete proof
// file against tapd's chain backend and asset transition verifier.
type ProofVerifier interface {
	policy.TapdInfoClient

	VerifyProof(context.Context, *taprpc.ProofFile,
		...grpc.CallOption) (*taprpc.VerifyProofResponse, error)
}

var _ ProofVerifier = (taprpc.TaprootAssetsClient)(nil)

// VerifyProofFile accepts a counterparty's HTLC from its complete proof file.
// It requires a genesis-rooted history without ownership challenges, in every
// nested input too, and the network's Taproot Assets v0.8.5 activation rules
// for every proof, then asks tapd to fully verify the file. The terminal proof
// must be anchored at the expected outpoint and is bound to this contract by
// ValidateProof before it is returned. The caller must still enforce
// confirmation depth and check that the outpoint is unspent. Even when
// re-verifying an existing swap, untrusted history acceptance requires a tapd
// that satisfies the shared version floor. Local claim/refund signing remains
// available with a previously verified proof.
func (s *SwapKit) VerifyProofFile(ctx context.Context, verifier ProofVerifier,
	proofFile *taprpc.ProofFile, expectedOutpoint *wire.OutPoint) (
	*proof.Proof, error) {

	if s == nil {
		return nil, fmt.Errorf("swap kit is required")
	}
	if verifier == nil {
		return nil, fmt.Errorf("proof verifier is required")
	}
	if proofFile == nil || len(proofFile.RawProofFile) == 0 {
		return nil, fmt.Errorf("proof file is required")
	}
	if expectedOutpoint == nil {
		return nil, fmt.Errorf("expected HTLC outpoint is required")
	}

	rawProof := proofFile.RawProofFile
	if err := policy.CheckProofFileSize(rawProof); err != nil {
		return nil, fmt.Errorf("invalid HTLC proof file: %w", err)
	}

	if err := policy.RequireOnchainSwapTapd(ctx, verifier); err != nil {
		return nil, err
	}

	// Check and verify a private copy, so the caller's buffer cannot change
	// between the structural checks and tapd's verification.
	proofFileCopy := &taprpc.ProofFile{
		RawProofFile: bytes.Clone(rawProof),
		GenesisPoint: proofFile.GenesisPoint,
	}
	decodedFile, err := proof.DecodeFile(proofFileCopy.RawProofFile)
	if err != nil {
		return nil, fmt.Errorf("unable to decode HTLC proof file: %w",
			err)
	}
	err = policy.ValidateProvenance(ctx, decodedFile, &s.addressParams)
	if err != nil {
		return nil, fmt.Errorf("invalid HTLC proof history: %w", err)
	}
	verifyResponse, err := verifier.VerifyProof(ctx, proofFileCopy)
	if err != nil {
		return nil, fmt.Errorf("unable to verify HTLC proof file: %w",
			err)
	}
	if verifyResponse == nil || !verifyResponse.Valid {
		return nil, fmt.Errorf("invalid HTLC proof file")
	}

	htlcProof, err := decodedFile.LastProof()
	if err != nil {
		return nil, fmt.Errorf("unable to read terminal HTLC proof: %w",
			err)
	}
	if htlcProof.OutPoint() != *expectedOutpoint {
		return nil, fmt.Errorf(
			"HTLC proof outpoint mismatch: expected %v, got %v",
			*expectedOutpoint, htlcProof.OutPoint(),
		)
	}
	if err := s.ValidateProof(htlcProof); err != nil {
		return nil, err
	}

	return htlcProof, nil
}

// validateAsset binds an asset leaf to the exact HTLC identity, amount, and
// script policy.
func (s *SwapKit) validateAsset(htlcAsset *asset.Asset) error {
	if err := s.validateScripts(); err != nil {
		return err
	}
	if s.amount == 0 {
		return fmt.Errorf("asset amount must be positive")
	}
	if htlcAsset == nil {
		return fmt.Errorf("asset is required")
	}

	proofID := htlcAsset.Genesis.ID()
	if !bytes.Equal(s.assetID[:], proofID[:]) {
		return fmt.Errorf("asset proof ID does not match swap")
	}
	if htlcAsset.Amount != s.amount {
		return fmt.Errorf("asset proof amount does not match swap")
	}
	if !policy.SupportsAssetVersion(htlcAsset.Version) {
		return fmt.Errorf("unsupported legacy deposit asset version")
	}
	if htlcAsset.ScriptVersion != asset.ScriptV0 {
		return fmt.Errorf("asset proof script version does not match swap")
	}
	if htlcAsset.LockTime != 0 || htlcAsset.RelativeLockTime != 0 {
		return fmt.Errorf("asset proof contains an unexpected locktime")
	}

	expectedScriptKey, _, _, _, err := CreateOpTrueLeaf()
	if err != nil {
		return err
	}
	expectedScriptKey = asset.NewScriptKey(expectedScriptKey.PubKey)
	if htlcAsset.ScriptKey.PubKey == nil ||
		!htlcAsset.ScriptKey.PubKey.IsEqual(expectedScriptKey.PubKey) {

		return fmt.Errorf("asset proof script key does not match swap")
	}

	return nil
}

type validatedSweep struct {
	assetInputIndex int
	prevOutputs     []*wire.TxOut
	assetRoot       []byte
}

// SpendWitness binds a witness stack to the exact transaction input selected
// by the proof outpoint.
type SpendWitness struct {
	InputIndex uint32
	Witness    wire.TxWitness
}

// findUniqueInput returns the sole transaction input that spends outpoint.
func findUniqueInput(tx *wire.MsgTx, outpoint wire.OutPoint) (int, error) {
	inputIndex := -1
	for idx, txIn := range tx.TxIn {
		if txIn == nil {
			return 0, fmt.Errorf("sweep input %d is nil", idx)
		}
		if txIn.PreviousOutPoint != outpoint {
			continue
		}
		if inputIndex >= 0 {
			return 0, fmt.Errorf("proof outpoint appears more than once")
		}

		inputIndex = idx
	}
	if inputIndex < 0 {
		return 0, fmt.Errorf("asset input does not spend proof outpoint")
	}

	return inputIndex, nil
}

// validateSweep binds a PSBT input and witness UTXO to the HTLC proof and
// reconstructs the exact Taproot output being spent. Unlike acceptance, it
// allows co-anchored leaves: the signer's transfer must then supply them,
// since the transfer validator rebuilds the complete input commitment.
func (s *SwapKit) validateSweep(assetProof *proof.Proof,
	sweepPacket *psbt.Packet) (*validatedSweep, error) {

	if assetProof == nil {
		return nil, fmt.Errorf("asset proof is required")
	}
	if err := policy.ValidateTransitionVersion(assetProof.Version); err != nil {
		return nil, err
	}
	if err := s.validateAsset(&assetProof.Asset); err != nil {
		return nil, err
	}
	assetCommitment, err := assetProof.VerifyProofs()
	if err != nil {
		return nil, fmt.Errorf("invalid asset proof: %w", err)
	}
	if err := policy.ValidateCommitmentVersion(assetCommitment.Version); err != nil {
		return nil, err
	}
	assetRootHash := assetCommitment.TapscriptRoot(nil)
	assetRoot := append([]byte(nil), assetRootHash[:]...)
	if sweepPacket == nil || sweepPacket.UnsignedTx == nil {
		return nil, fmt.Errorf("sweep PSBT is required")
	}
	if len(sweepPacket.UnsignedTx.TxIn) == 0 ||
		len(sweepPacket.UnsignedTx.TxIn) != len(sweepPacket.Inputs) {

		return nil, fmt.Errorf("sweep PSBT input metadata is incomplete")
	}
	if sweepPacket.UnsignedTx.Version < 2 {
		return nil, fmt.Errorf("sweep transaction must use version 2")
	}

	proofOutpoint := assetProof.OutPoint()
	assetInputIndex, err := findUniqueInput(
		sweepPacket.UnsignedTx, proofOutpoint,
	)
	if err != nil {
		return nil, err
	}
	prevOutputs := make([]*wire.TxOut, len(sweepPacket.Inputs))
	for idx := range sweepPacket.Inputs {
		witnessUtxo := sweepPacket.Inputs[idx].WitnessUtxo
		if witnessUtxo == nil {
			return nil, fmt.Errorf(
				"sweep input %d requires a witness UTXO", idx,
			)
		}
		prevOutputs[idx] = &wire.TxOut{
			Value:    witnessUtxo.Value,
			PkScript: append([]byte(nil), witnessUtxo.PkScript...),
		}
	}
	siblingPreimage, err := s.GetSiblingPreimage()
	if err != nil {
		return nil, err
	}
	siblingHash, err := siblingPreimage.TapHash()
	if err != nil {
		return nil, err
	}
	internalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}
	expectedPkScript, err := tapscript.PayToAddrScript(
		*internalKey, siblingHash, *assetCommitment,
	)
	if err != nil {
		return nil, err
	}
	proofOutputIndex := int(proofOutpoint.Index)
	if proofOutputIndex >= len(assetProof.AnchorTx.TxOut) {
		return nil, fmt.Errorf("proof output index is out of bounds")
	}
	proofTxOut := assetProof.AnchorTx.TxOut[proofOutputIndex]
	if proofTxOut == nil {
		return nil, fmt.Errorf("proof anchor output is nil")
	}
	assetPrevOut := prevOutputs[assetInputIndex]
	if proofTxOut.Value != assetPrevOut.Value ||
		!bytes.Equal(proofTxOut.PkScript, assetPrevOut.PkScript) {

		return nil, fmt.Errorf("asset prevout does not match proof anchor")
	}
	if !bytes.Equal(expectedPkScript, assetPrevOut.PkScript) {
		return nil, fmt.Errorf("asset input script does not match proof")
	}

	return &validatedSweep{
		assetInputIndex: assetInputIndex,
		prevOutputs:     prevOutputs,
		assetRoot:       assetRoot,
	}, nil
}

// AssetInputIndex returns the unique input that spends the proof outpoint. It
// validates all PSBT prevouts and the proof-bound anchor output before exposing
// the index. Callers use this index to apply SuccessSpendConstraints or
// TimeoutSpendConstraints before signing any inputs.
func (s *SwapKit) AssetInputIndex(assetProof *proof.Proof,
	sweepPacket *psbt.Packet) (uint32, error) {

	sweep, err := s.validateSweep(assetProof, sweepPacket)
	if err != nil {
		return 0, err
	}

	return uint32(sweep.assetInputIndex), nil
}

// validateSequence requires the proof-selected input to use the contract's
// sequence value before a witness is signed.
func validateSequence(sweep *validatedSweep, sweepPacket *psbt.Packet,
	required uint32) error {

	sequence := sweepPacket.UnsignedTx.TxIn[sweep.assetInputIndex].Sequence
	if sequence != required {
		return fmt.Errorf(
			"asset input sequence must be %d, got %d",
			required, sequence,
		)
	}

	return nil
}

// verifyTapscriptSignature checks a signer response against the exact input,
// prevouts, script, and public key used by an HTLC spend.
func verifyTapscriptSignature(tx *wire.MsgTx, sweep *validatedSweep,
	witnessScript []byte, pubKey *btcec.PublicKey, signature []byte) error {

	parsedSignature, err := schnorr.ParseSignature(signature)
	if err != nil {
		return fmt.Errorf("signer returned an invalid Schnorr signature: %w",
			err)
	}

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for idx, txIn := range tx.TxIn {
		prevOutFetcher.AddPrevOut(
			txIn.PreviousOutPoint, sweep.prevOutputs[idx],
		)
	}
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	tapLeaf := txscript.NewBaseTapLeaf(witnessScript)
	sigHash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, tx,
		sweep.assetInputIndex, prevOutFetcher, tapLeaf,
	)
	if err != nil {
		return fmt.Errorf("unable to calculate asset input sighash: %w", err)
	}
	if !parsedSignature.Verify(sigHash, pubKey) {
		return fmt.Errorf("signer returned a signature for the wrong key")
	}

	return nil
}

// CreatePreimageWitness signs the exact proof-bound asset input and returns a
// success-path witness in the legacy stack order. The caller must set the
// proof-selected input's sequence to SuccessSequence before calling this
// method; the PSBT is never mutated here. The transfer must contain the
// caller-approved, complete active and passive asset packets.
func (s *SwapKit) CreatePreimageWitness(ctx context.Context,
	signer lndclient.SignerClient, htlcProof *proof.Proof,
	sweepBtcPacket *psbt.Packet, keyLocator keychain.KeyLocator,
	preimage lntypes.Preimage, transfer *assetsweep.Transfer) (
	*SpendWitness, error) {

	if signer == nil {
		return nil, fmt.Errorf("signer is required")
	}
	if preimage.Hash() != s.swapHash {
		return nil, fmt.Errorf("preimage does not match swap hash")
	}

	sweep, err := s.validateSweep(htlcProof, sweepBtcPacket)
	if err != nil {
		return nil, err
	}
	if err := validateSequence(
		sweep, sweepBtcPacket, SuccessSequence,
	); err != nil {
		return nil, err
	}

	if err := transfer.Validate(htlcProof, sweepBtcPacket); err != nil {
		return nil, err
	}

	successScript, err := s.GetSuccessScript()
	if err != nil {
		return nil, err
	}

	signDesc := &lndclient.SignDescriptor{
		KeyDesc:       keychain.KeyDescriptor{KeyLocator: keyLocator},
		SignMethod:    input.TaprootScriptSpendSignMethod,
		WitnessScript: successScript,
		Output:        sweep.prevOutputs[sweep.assetInputIndex],
		InputIndex:    sweep.assetInputIndex,
	}
	sigs, err := signer.SignOutputRaw(
		ctx, sweepBtcPacket.UnsignedTx,
		[]*lndclient.SignDescriptor{signDesc}, sweep.prevOutputs,
	)
	if err != nil {
		return nil, err
	}
	if len(sigs) != 1 || len(sigs[0]) == 0 {
		return nil, fmt.Errorf("signer returned an invalid signature set")
	}
	if err := verifyTapscriptSignature(
		sweepBtcPacket.UnsignedTx, sweep, successScript,
		s.receiverScriptKey, sigs[0],
	); err != nil {
		return nil, err
	}

	successControlBlock, err := s.GenSuccessBtcControlBlock(
		sweep.assetRoot,
	)
	if err != nil {
		return nil, err
	}
	controlBlockBytes, err := successControlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return &SpendWitness{
		InputIndex: uint32(sweep.assetInputIndex),
		Witness: wire.TxWitness{
			preimage[:], sigs[0], successScript, controlBlockBytes,
		},
	}, nil
}

// CreateTimeoutWitness signs the exact proof-bound asset input and returns a
// timeout-path witness in the legacy stack order. The caller must set the
// proof-selected input's sequence and transaction locktime according to
// TimeoutSpendConstraints before signing any inputs. The PSBT is never mutated
// here. The transfer must contain the caller-approved, complete active and
// passive asset packets.
func (s *SwapKit) CreateTimeoutWitness(ctx context.Context,
	signer lndclient.SignerClient, htlcProof *proof.Proof,
	sweepBtcPacket *psbt.Packet, keyLocator keychain.KeyLocator,
	transfer *assetsweep.Transfer) (
	*SpendWitness, error) {

	if signer == nil {
		return nil, fmt.Errorf("signer is required")
	}

	sweep, err := s.validateSweep(htlcProof, sweepBtcPacket)
	if err != nil {
		return nil, err
	}
	if err := validateSequence(
		sweep, sweepBtcPacket, s.TimeoutSpendConstraints().Sequence,
	); err != nil {
		return nil, err
	}
	if s.policy == AssetHTLCCLTVV1 {
		lockTime := sweepBtcPacket.UnsignedTx.LockTime
		if lockTime < s.refundHeight ||
			lockTime >= txscript.LockTimeThreshold {

			return nil, fmt.Errorf("timeout locktime must be a block "+
				"height at least %d, got %d", s.refundHeight, lockTime)
		}
	}

	if err := transfer.Validate(htlcProof, sweepBtcPacket); err != nil {
		return nil, err
	}

	timeoutScript, err := s.GetTimeoutScript()
	if err != nil {
		return nil, err
	}

	signDesc := &lndclient.SignDescriptor{
		KeyDesc:       keychain.KeyDescriptor{KeyLocator: keyLocator},
		SignMethod:    input.TaprootScriptSpendSignMethod,
		WitnessScript: timeoutScript,
		Output:        sweep.prevOutputs[sweep.assetInputIndex],
		InputIndex:    sweep.assetInputIndex,
	}
	sigs, err := signer.SignOutputRaw(
		ctx, sweepBtcPacket.UnsignedTx,
		[]*lndclient.SignDescriptor{signDesc}, sweep.prevOutputs,
	)
	if err != nil {
		return nil, err
	}
	if len(sigs) != 1 || len(sigs[0]) == 0 {
		return nil, fmt.Errorf("signer returned an invalid signature set")
	}
	if err := verifyTapscriptSignature(
		sweepBtcPacket.UnsignedTx, sweep, timeoutScript,
		s.senderScriptKey, sigs[0],
	); err != nil {
		return nil, err
	}

	timeoutControlBlock, err := s.GenTimeoutBtcControlBlock(
		sweep.assetRoot,
	)
	if err != nil {
		return nil, err
	}
	controlBlockBytes, err := timeoutControlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return &SpendWitness{
		InputIndex: uint32(sweep.assetInputIndex),
		Witness: wire.TxWitness{
			sigs[0], timeoutScript, controlBlockBytes,
		},
	}, nil
}
