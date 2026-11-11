package tapsdk

import (
	"bytes"
	"context"

	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
)

// IsGroupMember checks the fungible asset's declared group identity. It does
// not verify issuance authorization or replace full proof verification.
func IsGroupMember(a *asset.Asset, key []byte) bool {
	return a != nil && a.Type == asset.Normal && a.GroupKey != nil &&
		SameGroupKey(a.GroupKey.GroupPubKey.SerializeCompressed(), key)
}

// VerifyGroupMembership verifies an anchor-first set of genesis proofs for
// the required fungible issuances. The verifier authenticates each proof with
// tapd; matching a group key alone is insufficient. Callers supply the required
// issuances and their aggregate proof-byte limit, independently of peer input.
// Node errors remain distinguishable from invalid evidence.
func VerifyGroupMembership(ctx context.Context, verifier RPCProofVerifier,
	params *address.ChainParams, groupKey []byte, required []asset.ID,
	issuanceProofs [][]byte, maxProofBytes int) error {

	if verifier == nil || !SameGroupKey(groupKey, groupKey) ||
		len(required) == 0 || maxProofBytes <= 0 {

		return swapkit.ErrInvalid
	}
	needed := make(map[asset.ID]bool, len(required))
	for _, id := range required {
		if id == asset.ZeroID {
			return swapkit.ErrInvalid
		}
		needed[id] = true
	}
	if len(issuanceProofs) == 0 || len(issuanceProofs) > len(needed)+1 {
		return swapkit.ErrInvalid
	}
	var total int
	for i, raw := range issuanceProofs {
		if len(raw) == 0 || len(raw) > maxProofBytes-total {
			return swapkit.ErrInvalid
		}
		total += len(raw)
		p, err := proof.Decode(raw)
		if err != nil || !p.Asset.HasGenesisWitnessForGroup() ||
			!IsGroupMember(&p.Asset, groupKey) ||
			(i == 0) != (p.GroupKeyReveal != nil) {

			return swapkit.ErrInvalid
		}
		id := p.Asset.ID()
		if i > 0 && !needed[id] {
			return swapkit.ErrInvalid
		}
		delete(needed, id)
		file, err := proof.NewFile(proof.V0, *p)
		if err != nil {
			return swapkit.ErrInvalid
		}
		if err := assetpolicy.ValidateProvenance(ctx, file, params); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return swapkit.ErrInvalid
		}
		data, err := proof.EncodeFile(file)
		if err != nil {
			return swapkit.ErrInvalid
		}
		result, err := verifier.VerifyProof(ctx, &taprpc.ProofFile{RawProofFile: data})
		if err != nil {
			return err
		}
		a := result.GetDecodedProof().GetAsset()
		if !result.GetValid() || !bytes.Equal(a.GetAssetGenesis().GetAssetId(), id[:]) ||
			!SameGroupKey(a.GetAssetGroup().GetTweakedGroupKey(), groupKey) {

			return swapkit.ErrInvalid
		}
	}
	if len(needed) != 0 {
		return swapkit.ErrInvalid
	}
	return nil
}
