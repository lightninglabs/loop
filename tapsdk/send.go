package tapsdk

import "errors"

// ErrMixedRecipientAmounts is returned by the low-level SendAsset when a
// single request mixes recipients with explicit amounts and recipients relying
// on address-embedded amounts. tapd's wire format does not support mixing the
// two paths; Wallet.SendMulti normalises such inputs before reaching the
// low-level RPC.
var ErrMixedRecipientAmounts = errors.New(
	"recipients mix explicit and embedded amounts",
)

// SendAssetRequest specifies a low-level one-shot address-based send.
//
// Recipients carries every destination. Each recipient either has an explicit
// amount or uses the address's embedded amount. tapd exposes two mutually
// exclusive wire paths for these, so callers must not mix explicit and embedded
// amounts in a single call; Client.SendAsset returns ErrMixedRecipientAmounts
// if they do. The higher-level Wallet.SendMulti normalises mixed inputs before
// reaching this layer.
type SendAssetRequest struct {
	// Recipients is the list of send destinations.
	Recipients []Recipient

	// FeeRate is the optional target fee rate for the anchor transaction.
	// Zero uses the daemon default.
	FeeRate FeeRate

	// Label is an optional short label for tracking the send.
	Label string

	// SkipProofCourierPingCheck skips the proof courier connectivity
	// check.
	SkipProofCourierPingCheck bool
}
