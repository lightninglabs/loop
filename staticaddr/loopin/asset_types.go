package loopin

import (
	"github.com/lightninglabs/loop/assets"
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
	Request         AssetSwapRequest `json:"Request"`
	Offer           swapkit.Offer    `json:"Offer"`
	RecoveryRequest []byte           `json:"RecoveryRequest"`
	Fallback        []byte           `json:"Fallback"`
	Accepted        *assets.Evidence `json:"Accepted"`
}
