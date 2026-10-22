package tapsdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"

	"github.com/btcsuite/btcd/psbt/v2"

	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/loop/internal/assetproof"

	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
)

// ConfirmPackageInputs replaces a pre-signed child's compact input histories
// with verified confirmed histories. The saved digest identifies independently
// approved package terms. The package must also carry complete, valid Bitcoin
// witnesses, which are checked separately by the native SDK. Only proof
// metadata changes: Bitcoin signatures, asset state, allocations, commitments,
// fees and signing policy remain unchanged. Confirmation evidence is checked
// against both the current chain and the authenticated SDK proof verifier.
//
// The returned native package has a new CommittedPackageDigest because its
// proof histories changed. Persist the association with savedDigest and the
// updated package before publishing. The FSM must import each returned native
// ProofFile through wallet.ImportProofFile before PublishCustomAnchorTransfer;
// tapd's proof archive needs those histories to construct successor proofs.
// Import and publication retry/ambiguity remain owned by the caller. The chain
// must also implement PrevoutSource for authenticated Bitcoin funding outputs.
func ConfirmPackageInputs(ctx context.Context, wallet *sdk.Wallet, chain Chain,
	minConfs uint32, approved *sdk.CustomAnchorTransferPackage,
	savedDigest sdk.Hash, confirmations map[sdk.Hash]sdk.AnchorConfirmation,
	bindings []InputBinding) (*sdk.CustomAnchorTransferPackage,
	[]*sdk.ProofFile, error) {

	if approved == nil || savedDigest == (sdk.Hash{}) ||
		approved.CommittedPackageDigest != savedDigest {

		return nil, nil, fmt.Errorf(
			"child differs from saved committed digest",
		)
	}
	if err := CheckSigningPolicy(approved); err != nil {
		return nil, nil, err
	}
	original, err := approved.WithFinalAnchorPSBT(approved.AnchorPsbt)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"child Bitcoin authorization: %w", err,
		)
	}
	if minConfs == 0 {
		return nil, nil, fmt.Errorf("confirmation policy is required")
	}
	if chain == nil {
		return nil, nil, fmt.Errorf("current chain backend is required")
	}
	if len(bindings) != len(original.Inputs) {
		return nil, nil, fmt.Errorf(
			"every asset input needs its agreed binding",
		)
	}
	byID := make(map[string]InputBinding, len(bindings))
	for _, binding := range bindings {
		if binding.ID == "" {
			return nil, nil, fmt.Errorf(
				"asset input binding ID is required",
			)
		}
		if _, exists := byID[binding.ID]; exists {
			return nil, nil, fmt.Errorf(
				"duplicate asset input binding",
			)
		}
		byID[binding.ID] = binding.clone()
	}
	confirmations, err = cloneConfirmations(confirmations)
	if err != nil {
		return nil, nil, err
	}
	if err := CheckNodeVersion(ctx, wallet); err != nil {
		return nil, nil, err
	}
	anchor, err := DecodePSBT(original.CommittedAnchorPsbt)
	if err != nil {
		return nil, nil, err
	}
	updated := original.Clone()
	proofs := make([]*proof.Proof, len(original.Inputs))
	raws := make([][]byte, len(original.Inputs))
	var totalBytes uint64
	for idx, input := range original.Inputs {
		binding, ok := byID[input.LogicalInputID]
		if !ok {
			return nil, nil, fmt.Errorf(
				"asset input binding is missing",
			)
		}
		delete(byID, input.LogicalInputID)
		raw, source, err := confirmInputSource(input.ProofSource,
			confirmations)
		if err != nil {
			return nil, nil, err
		}
		size := uint64(len(raw))
		if size > assetpolicy.MaxProvenanceBytes-totalBytes {
			return nil, nil, fmt.Errorf(
				"confirmed histories exceed size budget",
			)
		}
		totalBytes += size
		err = checkProvenance(ctx, wallet, raw)
		if err != nil {
			return nil, nil, err
		}
		p, err := LastProof(raw)
		if err != nil {
			return nil, nil, err
		}
		if err := binding.validate(p); err != nil {
			return nil, nil, err
		}
		if err := checkPathSigning(binding, anchor,
			original.SigningPlans); err != nil {
			return nil, nil, err
		}
		if err := replaceInputProof(updated, input, p); err != nil {
			return nil, nil, err
		}
		updated.Inputs[idx].ProofSource = source
		proofs[idx], raws[idx] = p, raw
	}
	if err := assetproof.VerifyCompleteAnchors(proofs); err != nil {
		return nil, nil, err
	}
	if err := checkConfirmedPackageCurrent(ctx, wallet, chain, minConfs, proofs, anchor); err != nil {
		return nil, nil, err
	}
	// Verification reuse is scoped to this operation and exact history
	// bytes. Every binding and current outpoint is checked independently.
	verified := make(map[[32]byte]bool)
	var files []*sdk.ProofFile
	for idx, p := range proofs {
		if err := checkConfirmation(ctx, chain,
			minConfs, p, true); err != nil {
			return nil, nil, err
		}
		digest := sha256.Sum256(raws[idx])
		if !verified[digest] {
			result, err := wallet.Client().VerifyProof(ctx,
				bytes.Clone(raws[idx]))
			if err != nil {
				return nil, nil, err
			}
			if err := checkVerifiedSummary(result, p); err != nil {
				return nil, nil, err
			}
			verified[digest] = true
			genesis := p.Asset.Genesis.FirstPrevOut
			files = append(files, &sdk.ProofFile{
				RawProofFile: bytes.Clone(raws[idx]),
				GenesisPoint: sdk.Outpoint{
					Txid:  sdk.Hash(genesis.Hash),
					Index: genesis.Index,
				},
			})
		}
		if err := checkConfirmation(ctx, chain,
			minConfs, p, true); err != nil {
			return nil, nil, err
		}
	}
	if err := checkConfirmedPackageCurrent(ctx, wallet, chain, minConfs, proofs, anchor); err != nil {
		return nil, nil, err
	}
	updated, err = updated.Seal()
	if err != nil {
		return nil, nil, fmt.Errorf(
			"seal confirmed input metadata: %w", err,
		)
	}
	updated, err = updated.WithFinalAnchorPSBT(original.AnchorPsbt)
	if err != nil {
		return nil, nil, err
	}
	if err := checkConfirmationOnly(original, updated); err != nil {
		return nil, nil, err
	}
	if err := checkConfirmedPackageCurrent(ctx, wallet, chain, minConfs, proofs, anchor); err != nil {
		return nil, nil, err
	}
	return updated, files, nil
}

func confirmInputSource(source sdk.CustomAnchorProofSourceSummary,
	confirmations map[sdk.Hash]sdk.AnchorConfirmation) ([]byte,
	sdk.CustomAnchorProofSourceSummary, error) {

	if source.Kind == sdk.CustomAnchorProofSourceConfirmedFile {
		return bytes.Clone(source.Blob), source, nil
	}
	if source.Kind != sdk.CustomAnchorProofSourceCompactPath {
		return nil, source, fmt.Errorf("unsupported input proof source")
	}
	var path sdk.AssetProofPath
	if err := path.UnmarshalBinary(source.Blob); err != nil {
		return nil, source, err
	}
	raw, err := path.ConfirmProofFile(confirmations)
	if err != nil {
		return nil, source, err
	}
	// A zero-step native path is a confirmed proof source accepted by the
	// SDK publisher. Its public ContentID avoids duplicating a private hash
	// domain or introducing another package/proof serialization.
	confirmed := sdk.AssetProofPath{ConfirmedBaseProof: raw}
	id, err := confirmed.ContentID()
	if err != nil {
		return nil, source, err
	}
	blob, err := confirmed.MarshalBinary()
	if err != nil {
		return nil, source, err
	}
	return raw, sdk.CustomAnchorProofSourceSummary{
		Kind:      sdk.CustomAnchorProofSourceCompactPath,
		ContentID: id, Blob: blob,
	}, nil
}

func replaceInputProof(pkg *sdk.CustomAnchorTransferPackage,
	input sdk.CustomAnchorAssetInputSummary, confirmed *proof.Proof) error {

	packets := pkg.ActiveVirtualPsbts
	if input.PacketRole == sdk.CustomAnchorPacketRolePassive {
		packets = pkg.PassiveVirtualPsbts
	}
	if input.PacketIndex >= uint32(len(packets)) {
		return fmt.Errorf("asset packet index is out of range")
	}
	packet, err := tappsbt.Decode(packets[input.PacketIndex])
	if err != nil {
		return err
	}
	if input.VirtualInputIndex >= uint32(len(packet.Inputs)) {
		return fmt.Errorf("asset input index is out of range")
	}
	selected := packet.Inputs[input.VirtualInputIndex]
	if selected.Proof == nil ||
		selected.Proof.OutPoint() != confirmed.OutPoint() {

		return fmt.Errorf("confirmed proof selects another anchor")
	}
	var oldAsset, newAsset bytes.Buffer
	if err := selected.Proof.Asset.Encode(&oldAsset); err != nil {
		return err
	}
	if err := confirmed.Asset.Encode(&newAsset); err != nil {
		return err
	}
	if !bytes.Equal(oldAsset.Bytes(), newAsset.Bytes()) {
		return fmt.Errorf("confirmation changes the input asset state")
	}
	selected.Proof = confirmed
	packets[input.PacketIndex], err = tappsbt.Encode(packet)
	return err
}

// checkConfirmationOnly compares native snapshots after removing only the
// permitted input-proof changes and the digests that necessarily cover them.
func checkConfirmationOnly(original,
	updated *sdk.CustomAnchorTransferPackage) error {

	if original.UnsignedTxDigest != updated.UnsignedTxDigest ||
		!bytes.Equal(original.AnchorPsbt, updated.AnchorPsbt) ||
		!bytes.Equal(original.CommittedAnchorPsbt,
			updated.CommittedAnchorPsbt) {

		return fmt.Errorf(
			"confirmation changes the approved Bitcoin transaction",
		)
	}
	a, b := original.Clone(), updated.Clone()
	if len(a.Inputs) != len(b.Inputs) {
		return fmt.Errorf("confirmation changes asset input count")
	}
	for idx := range b.Inputs {
		b.Inputs[idx].ProofSource = a.Inputs[idx].ProofSource
	}
	for _, pair := range [][2][][]byte{
		{a.ActiveVirtualPsbts, b.ActiveVirtualPsbts},
		{a.PassiveVirtualPsbts, b.PassiveVirtualPsbts},
	} {
		if len(pair[0]) != len(pair[1]) {
			return fmt.Errorf(
				"confirmation changes virtual packet count",
			)
		}
		for idx, originalRaw := range pair[0] {
			before, err := tappsbt.Decode(originalRaw)
			if err != nil {
				return err
			}
			after, err := tappsbt.Decode(pair[1][idx])
			if err != nil {
				return err
			}
			if len(before.Inputs) != len(after.Inputs) {
				return fmt.Errorf(
					"confirmation changes virtual inputs",
				)
			}
			for j := range after.Inputs {
				after.Inputs[j].Proof = before.Inputs[j].Proof
			}
			oldRaw, err := tappsbt.Encode(before)
			if err != nil {
				return err
			}
			newRaw, err := tappsbt.Encode(after)
			if err != nil {
				return err
			}
			if !bytes.Equal(oldRaw, newRaw) {
				return fmt.Errorf(
					"confirmation changes virtual state",
				)
			}
			pair[1][idx] = bytes.Clone(originalRaw)
		}
	}
	b.CommittedPackageDigest = a.CommittedPackageDigest
	b.PackageDigest = a.PackageDigest
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf(
			"confirmation changes approved package policy",
		)
	}
	return nil
}

// checkConfirmedPackageCurrent includes independent fee inputs, which are not
// selected by asset proof bindings, and repeats the live-node compatibility gate.
func checkConfirmedPackageCurrent(ctx context.Context, wallet *sdk.Wallet,
	chain Chain, minConfs uint32, proofs []*proof.Proof, anchor *psbt.Packet) error {

	if err := CheckNodeVersion(ctx, wallet); err != nil {
		return err
	}
	for _, p := range proofs {
		if err := checkConfirmation(ctx, chain, minConfs, p, true); err != nil {
			return err
		}
	}
	source, ok := chain.(PrevoutSource)
	if !ok {
		return fmt.Errorf("confirmation handoff requires authenticated Bitcoin prevouts")
	}
	raw, err := EncodePSBT(anchor)
	if err != nil {
		return err
	}
	if err := CheckFundingInputs(ctx, source, raw); err != nil {
		return err
	}
	return checkFundingCurrent(ctx, chain, anchor, nil)
}

func cloneConfirmations(confirmations map[sdk.Hash]sdk.AnchorConfirmation) (
	map[sdk.Hash]sdk.AnchorConfirmation, error) {

	var total int
	for _, c := range confirmations {
		if len(c.Block) > assetpolicy.MaxProvenanceBytes-total {
			return nil, fmt.Errorf("confirmation evidence exceeds size budget")
		}
		total += len(c.Block)
	}
	result := make(map[sdk.Hash]sdk.AnchorConfirmation, len(confirmations))
	for id, c := range confirmations {
		c.Block = bytes.Clone(c.Block)
		result[id] = c
	}
	return result, nil
}
