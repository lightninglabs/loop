package loopin

import (
	"github.com/lightninglabs/loop/assets/assetln"
	"github.com/lightninglabs/loop/swapkit"
)

// AssetSwapRequest fixes the native quantity and fees accepted by the user.
//
//nolint:tagliatelle // Preserve the existing recovery format.
type AssetSwapRequest struct {
	RequestID [32]byte `json:"RequestID"`
	GroupKey  [33]byte `json:"GroupKey"`
	Outpoint  string   `json:"Outpoint"`
	Amount    uint64   `json:"Amount"`
	MaxDebit  uint64   `json:"MaxDebit"`
	MaxFee    uint64   `json:"MaxFee"`
}

func validAssetSwapAmount(amount uint64, request AssetSwapRequest) bool {
	return request.Amount > 0 && request.Amount <= amount &&
		request.MaxDebit >= request.Amount &&
		amount-request.Amount <= request.MaxFee
}

// AssetSwapInfo contains the asset-specific contract and recovery material of
// a normal static Loop In. Its lifecycle is StaticAddressLoopIn.state.
//
//nolint:tagliatelle // Preserve the existing recovery format.
type AssetSwapInfo struct {
	Request         AssetSwapRequest     `json:"Request"`
	Offer           swapkit.Offer        `json:"Offer"`
	Invoice         *assetln.HoldInvoice `json:"Invoice"`
	RecoveryRequest []byte               `json:"RecoveryRequest"`
	Fallback        swapkit.Fallback     `json:"Fallback"`
	Accepted        *assetln.Evidence    `json:"Accepted"`
}

func (a *AssetSwapInfo) paymentTerms() assetln.Terms {
	t := a.Offer.Terms
	return assetln.Terms{
		Hash: a.Offer.Hash, GroupKey: t.GroupKey, Amount: t.PayoutAmount,
		MaxDebit: t.MaxDebit, RefundHeight: t.RefundHeight,
		RecoveryBlocks: t.RecoveryDelta, HoldExpiryDelta: 20,
		Deadline: t.OfferExpiry.Unix(),
	}
}

func (a *AssetSwapInfo) operation() swapkit.Operation {
	return swapkit.Operation{
		Offer: a.Offer, Fallback: a.Fallback,
		Preparation: swapkit.Preparation{RecoveryRequest: a.RecoveryRequest},
	}
}
