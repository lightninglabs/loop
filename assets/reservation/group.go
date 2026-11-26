package reservation

import (
	"context"
	"errors"

	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/loop/swapkit/tapsdk"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/asset"
)

// MaxGroupProofBytes leaves room in the ordinary quote RPC for both invoices
// and RFQs. Only the anchor and up to two distinct member issuances are needed.
const MaxGroupProofBytes = 3 << 20

// IsGroupMember checks the fungible asset's declared group identity. Proof
// verification is still required before trusting that declaration.
func IsGroupMember(a *asset.Asset, key []byte) bool {
	return tapsdk.IsGroupMember(a, key)
}

// VerifyGroup binds the quote's funding and payment issuances to the requested
// group before probing or paying. Swapkit authenticates the supplied evidence.
func (w *WalletVerifier) VerifyGroup(ctx context.Context,
	q *swapserverrpc.AssetReservationQuote) error {

	g := q.GetGroup()
	if w == nil || w.Tap == nil || len(g.GetGroupKey()) != 33 ||
		len(q.GetTerms().GetAssetId()) != 32 || len(g.GetPaymentAssetId()) != 32 ||
		len(g.GetIssuanceProofs()) == 0 || len(g.GetIssuanceProofs()) > 3 {

		return ErrInvalidReservation
	}
	err := tapsdk.VerifyGroupMembership(ctx, w.Tap, w.Params, g.GroupKey,
		[]asset.ID{asset.ID(q.Terms.AssetId), asset.ID(g.PaymentAssetId)},
		g.IssuanceProofs, MaxGroupProofBytes)
	if errors.Is(err, swapkit.ErrInvalid) {
		return ErrInvalidReservation
	}
	return err
}
