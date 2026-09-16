package payment

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// ReceiptTerms binds a receipt to its invoice and receiving asset channel.
type ReceiptTerms struct {
	Hash           [32]byte
	AssetID        [32]byte
	Amount         uint64
	PaymentAddress [32]byte
	AddIndex       uint64
	MaxParts       uint32
	ChannelID      uint64
}

// Incoming summarizes verified raw asset HTLCs, not auxiliary BTC accounting.
// Expiries includes every live part so the caller can apply its final gate.
type Incoming struct {
	AssetAmount uint64
	Parts       uint32
	Expiries    []uint32
}

// ValidateIncoming requires the exact asset amount from the exact RFQ in every
// accepted or settled part. tapd's normal-invoice rounding tolerance can settle
// an underpayment; that is not entitlement to full prepay credit or funding.
// The caller must save such anomalous settlement evidence for admin recovery.
// Expiry alone cannot invalidate a receipt already settled in the past.
func ValidateIncoming(invoice *lnrpc.Invoice,
	quote *rfqrpc.PeerAcceptedBuyQuote, terms ReceiptTerms) (Incoming, error) {

	var result Incoming
	if invoice == nil || terms.Hash == ([32]byte{}) ||
		terms.PaymentAddress == ([32]byte{}) || terms.AddIndex == 0 ||
		terms.MaxParts == 0 || terms.MaxParts > 16 || terms.ChannelID == 0 ||
		!bytes.Equal(invoice.RHash, terms.Hash[:]) ||
		!bytes.Equal(invoice.PaymentAddr, terms.PaymentAddress[:]) ||
		invoice.AddIndex != terms.AddIndex ||
		invoice.IsAmp || invoice.IsKeysend || invoice.CreationDate <= 0 ||
		invoice.ValueMsat <= 0 ||
		(invoice.State != lnrpc.Invoice_ACCEPTED &&
			invoice.State != lnrpc.Invoice_SETTLED) {

		return result, fmt.Errorf("invoice is not the bound asset payment")
	}
	// Reconstruct the amount at creation time, not today's exchange rate.
	msat, err := ReceivingAmount(quote, terms.AssetID, terms.Amount,
		time.Unix(invoice.CreationDate, 0))
	if err != nil || msat != uint64(invoice.ValueMsat) {
		return result, fmt.Errorf("invoice receiving quote mismatch")
	}
	seen := make(map[[2]uint64]struct{})
	for _, part := range invoice.Htlcs {
		if part == nil {
			return Incoming{}, fmt.Errorf("missing invoice HTLC")
		}
		if part.State == lnrpc.InvoiceHTLCState_CANCELED {
			continue
		}
		if (invoice.State == lnrpc.Invoice_ACCEPTED &&
			part.State != lnrpc.InvoiceHTLCState_ACCEPTED) ||
			(invoice.State == lnrpc.Invoice_SETTLED &&
				part.State != lnrpc.InvoiceHTLCState_SETTLED) {

			return Incoming{}, fmt.Errorf("inconsistent invoice HTLC set")
		}
		circuit := [2]uint64{part.ChanId, part.HtlcIndex}
		if _, ok := seen[circuit]; ok {
			return Incoming{}, fmt.Errorf("duplicate invoice HTLC")
		}
		seen[circuit] = struct{}{}
		result.Parts++
		if result.Parts > terms.MaxParts || part.ExpiryHeight <= 0 {
			return Incoming{}, fmt.Errorf("invalid asset payment parts")
		}
		if part.ChanId != terms.ChannelID {
			return Incoming{}, fmt.Errorf("unexpected receiving channel")
		}
		record, err := rfqmsg.HtlcFromCustomRecords(part.CustomRecords)
		if err != nil || record.RfqID.ValOpt().IsNone() {
			return Incoming{}, fmt.Errorf("missing asset HTLC quote")
		}
		id := record.RfqID.ValOpt().UnsafeFromSome()
		if !bytes.Equal(id[:], quote.Id) || len(record.Balances()) != 1 {
			return Incoming{}, fmt.Errorf("asset HTLC quote mismatch")
		}
		balance := record.Balances()[0]
		if balance == nil || balance.AssetID.Val != asset.ID(terms.AssetID) ||
			balance.Amount.Val == 0 ||
			balance.Amount.Val > math.MaxInt64-result.AssetAmount {

			return Incoming{}, fmt.Errorf("invalid asset HTLC balance")
		}
		result.AssetAmount += balance.Amount.Val
		result.Expiries = append(result.Expiries, uint32(part.ExpiryHeight))
	}
	if result.Parts == 0 || result.AssetAmount != terms.Amount {
		return Incoming{}, fmt.Errorf("asset payment amount mismatch")
	}
	return result, nil
}
