package tapsdk

import (
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

// HTLC constructs the existing V3 Bitcoin contract for the offered swap.
func HTLC(offer swapkit.Offer, c DepositContract) (*swapkit.SwapKit, error) {
	return swapkit.NewSwapKit(swapkit.SwapKitParams{
		Policy:    swapkit.SwapHTLCStaticLoopInV3,
		SenderKey: c.ClientKey, ReceiverKey: c.ServerKey,
		SenderInternalKey:   c.ClientKey,
		ReceiverInternalKey: c.ServerKey,
		PaymentHash:         offer.Hash,
		Expiry:              offer.Terms.RefundHeight,
	})
}

// fallbackTerms expresses a Loop In fallback as a generic HTLC transfer: the
// client's deposit is spent into an HTLC that the server claims.
func fallbackTerms(offer swapkit.Offer, d *DepositContext) (HTLCTerms, error) {
	c := d.Contract
	policy, err := HTLC(offer, c)
	if err != nil {
		return HTLCTerms{}, err
	}
	dk, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: c.ClientKey, ServerKey: c.ServerKey,
		CSVExpiry: c.CSV,
	})
	if err != nil {
		return HTLCTerms{}, err
	}

	return HTLCTerms{
		Funding: dk, Input: c.Output, InputProof: d.Deposit.Proof,
		HTLC: policy, SessionContext: offer.Hash[:],
	}, nil
}

// validateRecoveryPackage binds a native SDK transfer to the offered fallback.
func validateRecoveryPackage(offer swapkit.Offer, d *DepositContext,
	raw []byte) (*sdk.CustomAnchorTransferPackage, error) {

	terms, err := fallbackTerms(offer, d)
	if err != nil {
		return nil, err
	}

	return ValidateHTLCPackage(terms, raw)
}

// FallbackRequest constructs the unsigned SDK request for one validated offer.
// The fallback is a zero-fee version 3 transaction that moves the whole
// deposit anchor value into the HTLC and adds a pay-to-anchor output. Its
// publisher pays the fee with a child of that output and submits both as one
// package. maxSweepFee is the fee the HTLC output must still be able to pay
// for its later claim or refund.
func FallbackRequest(s *SwapContext, maxSweepFee uint64) (
	*sdk.CustomAnchorRequest, error) {

	terms, err := fallbackTerms(s.Offer, s.Deposit)
	if err != nil {
		return nil, err
	}

	return HTLCRequest(terms, maxSweepFee)
}
