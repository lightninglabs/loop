package reservation

import (
	"errors"
	"math"
)

// Terms records the asset amounts and lifetime offered for a reservation.
// The client initially fills only AssetID and Amount from the purchase request.
// The server preserves those values and fills Fee and the lifetime fields from
// its pricing and lifetime policies when creating the quote.
//
// The client validates and saves the returned terms before requesting approval.
// Approving the quote's hash accepts its asset fee and BTC prepay amount. The
// client saves the prepay routing cap and SkipProbe choice with the transition
// to payment. Recovery uses the saved terms, even if server policy has changed.
// The field comments describe the full flow; Validate alone checks only amounts
// and lifetime consistency.
type Terms struct {
	// AssetID identifies the asset requested by the client. The client
	// checks the quote against its request and the reservation proof against
	// this saved ID.
	AssetID [32]byte

	// Amount is the requested reservation principal, in indivisible asset
	// units. The client checks the quote against its request and requires
	// the reservation proof to contain this exact amount.
	Amount uint64

	// Fee is the server's service charge, in indivisible asset units. The
	// client pays it in full when buying the reservation. Under the current
	// contract, this prepayment covers the entire fee for the later swap.
	// The server keeps the prepayment if the reservation goes unused.
	//
	// For Amount = 10,000 and Fee = 10, the client pays 10 units upfront,
	// then 10,000 at swap execution to receive 10,000 on-chain. Routing and
	// miner fees are separate.
	//
	// The client accepts Fee by approving the exact quote and uses the
	// receiving RFQ to check the BTC prepay invoice. The server verifies that
	// it received exactly Fee asset units; the client requires the reported
	// prepay credit to match. The later swap applies that credit once.
	Fee uint64

	// CSVDelay is the server's quoted timeout delay, measured in blocks from
	// the first funding confirmation. The client checks the reservation proof
	// against a script built with this delay. Both parties use it to derive
	// the original timeout height; recovery does not restart the clock.
	CSVDelay uint32

	// RequiredConfirmations is the funding depth quoted by the server.
	// Each party checks the observed depth against this value before Ready.
	// The client uses the quoted depth; Validate imposes no minimum beyond
	// requiring a positive value that fits the reservation lifetime.
	RequiredConfirmations uint32

	// ExecutionDelta is the server's quoted margin between the execution
	// cutoff and CSV maturity. Both parties derive the cutoff from it.
	// The later swap must enforce that cutoff and its own claim-time checks.
	ExecutionDelta uint32

	// MinUsableBlocks is the server's promised initial usable window.
	// Before delivery, the server requires strictly more
	// than this many blocks before the execution cutoff. A recovering client
	// retains the quote but does not demand a fresh window after being offline.
	MinUsableBlocks uint32
}

// Validate rejects zero asset IDs, nonpositive amounts, signed-storage
// overflow, and CSV values outside the block-based BIP68 range. It also checks
// that confirmation depth, execution margin, and usable window fit the delay.
// It does not compare terms with the client's request or fee policy,
// enforce a fee percentage or client lifetime policy, or inspect a funded output.
func (t Terms) Validate() error {
	if t.AssetID == ([32]byte{}) || t.Amount == 0 || t.Fee == 0 ||
		t.Amount > math.MaxInt64 || t.Fee > math.MaxInt64 {

		return errors.New("invalid asset reservation amounts")
	}
	if t.Amount > math.MaxInt64-t.Fee {
		return errors.New("asset amount plus fee is out of range")
	}

	// A plain 16-bit sequence selects block-based CSV without disable or
	// time-based flags. Use wider arithmetic for untrusted policy values.
	required := uint64(t.RequiredConfirmations) + uint64(t.ExecutionDelta) +
		uint64(t.MinUsableBlocks)
	if t.CSVDelay == 0 || t.CSVDelay > math.MaxUint16 ||
		t.RequiredConfirmations == 0 || t.ExecutionDelta == 0 ||
		t.MinUsableBlocks == 0 || required > uint64(t.CSVDelay) {

		return errors.New("invalid asset reservation lifetime")
	}

	return nil
}
