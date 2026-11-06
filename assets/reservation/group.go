package reservation

import (
	"bytes"
	"context"

	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
)

// MaxGroupProofBytes leaves room in the ordinary quote RPC for both invoices
// and RFQs. Only the anchor and up to two distinct member issuances are needed.
const MaxGroupProofBytes = 3 << 20

// IsGroupMember requires a fungible issuance with the expected tweaked key.
func IsGroupMember(a *asset.Asset, key []byte) bool {
	return a != nil && a.Type == asset.Normal && a.GroupKey != nil &&
		SameGroupKey(a.GroupKey.GroupPubKey.SerializeCompressed(), key)
}

// VerifyGroup verifies membership before a group purchase may probe or pay.
// The first proof establishes the anchor for a client new to this group.
// Only genesis proofs are exchanged here; delivery verifies the full transfer.
func (w *WalletVerifier) VerifyGroup(ctx context.Context,
	q *swapserverrpc.AssetReservationQuote) error {

	g := q.GetGroup()
	if w.Tap == nil || len(g.GetGroupKey()) != 33 ||
		len(q.GetTerms().GetAssetId()) != 32 || len(g.PaymentAssetId) != 32 ||
		len(g.IssuanceProofs) == 0 || len(g.IssuanceProofs) > 3 {

		return ErrInvalidReservation
	}
	needed := map[asset.ID]bool{
		asset.ID(q.Terms.AssetId):  true,
		asset.ID(g.PaymentAssetId): true,
	}
	var total int
	for i, raw := range g.IssuanceProofs {
		if len(raw) == 0 || len(raw) > MaxGroupProofBytes-total {
			return ErrInvalidReservation
		}
		total += len(raw)
		p, err := proof.Decode(raw)
		if err != nil || !p.Asset.HasGenesisWitnessForGroup() ||
			!IsGroupMember(&p.Asset, g.GroupKey) ||
			(i == 0) != (p.GroupKeyReveal != nil) {

			return ErrInvalidReservation
		}
		id := p.Asset.ID()
		if i > 0 && !needed[id] {
			return ErrInvalidReservation
		}
		delete(needed, id)
		file, err := proof.NewFile(proof.V0, *p)
		if err != nil {
			return ErrInvalidReservation
		}
		if err := assetpolicy.ValidateProvenance(ctx, file, w.Params); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrInvalidReservation
		}
		data, err := proof.EncodeFile(file)
		if err != nil {
			return ErrInvalidReservation
		}
		result, err := w.Tap.VerifyProof(ctx, &taprpc.ProofFile{RawProofFile: data})
		if err != nil {
			return err
		}
		a := result.GetDecodedProof().GetAsset()
		if !result.GetValid() || !bytes.Equal(a.GetAssetGenesis().GetAssetId(), id[:]) ||
			!SameGroupKey(a.GetAssetGroup().GetTweakedGroupKey(), g.GroupKey) {

			return ErrInvalidReservation
		}
	}
	if len(needed) != 0 {
		return ErrInvalidReservation
	}
	return nil
}
