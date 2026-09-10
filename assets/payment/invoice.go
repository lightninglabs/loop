package payment

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

// InvoiceTerms binds BOLT11's Bitcoin amount to an independently agreed asset
// contract. PaymentAddress, when known, binds the saved invoice.
type InvoiceTerms struct {
	AssetID        [32]byte
	Amount         uint64
	Hash           [32]byte
	Payee          [33]byte
	PaymentAddress [32]byte
	MinFinalCLTV   uint64
	MinLifetime    time.Duration
}

// ValidateInvoice checks the signed invoice, not just the creation request.
// Invoice creation must use the supplied RFQ without negotiating another one.
// This function is an admission check; use raw receipt validation to reconcile
// a payment that already settled after its quote or invoice expired.
func ValidateInvoice(encoded string, quote *rfqrpc.PeerAcceptedBuyQuote,
	terms InvoiceTerms, params *chaincfg.Params, now time.Time) (
	*zpay32.Invoice, error) {

	if params == nil || terms.Hash == ([32]byte{}) ||
		terms.MinFinalCLTV == 0 || terms.MinFinalCLTV > math.MaxUint16 ||
		terms.MinLifetime <= 0 {

		return nil, fmt.Errorf("invalid invoice contract")
	}
	invoice, err := zpay32.Decode(encoded, params)
	if err != nil {
		return nil, err
	}
	expires := invoice.Timestamp.Add(invoice.Expiry())
	if !expires.After(now.Add(terms.MinLifetime)) ||
		invoice.MinFinalCLTVExpiry() < terms.MinFinalCLTV ||
		invoice.MinFinalCLTVExpiry() > math.MaxUint16 {

		return nil, fmt.Errorf("unsafe invoice lifetime")
	}
	expected, err := ReceivingAmount(quote, terms.AssetID, terms.Amount,
		expires)
	if err != nil {
		return nil, err
	}
	if invoice.PaymentHash == nil || *invoice.PaymentHash != terms.Hash ||
		invoice.Destination == nil || !bytes.Equal(
		invoice.Destination.SerializeCompressed(), terms.Payee[:],
	) || invoice.MilliSat == nil ||
		uint64(*invoice.MilliSat) != expected {

		return nil, fmt.Errorf("invoice does not match asset contract")
	}
	if invoice.PaymentAddr.IsNone() || invoice.Features == nil ||
		!invoice.Features.HasFeature(lnwire.PaymentAddrOptional) ||
		!invoice.Features.HasFeature(lnwire.TLVOnionPayloadOptional) ||
		invoice.Features.HasFeature(lnwire.AMPOptional) {

		return nil, fmt.Errorf("invoice requires non-AMP payment secret")
	}
	secret := invoice.PaymentAddr.UnsafeFromSome()
	if secret == ([32]byte{}) || (terms.PaymentAddress != ([32]byte{}) &&
		secret != terms.PaymentAddress) {

		return nil, fmt.Errorf("invoice payment secret mismatch")
	}
	if len(invoice.RouteHints) != 1 || len(invoice.RouteHints[0]) != 1 ||
		len(invoice.BlindedPaymentPaths) != 0 {

		return nil, fmt.Errorf("invoice must use one receiving edge hint")
	}
	peer, err := parsePeer(quote.Peer)
	if err != nil {
		return nil, err
	}
	hint := invoice.RouteHints[0][0]
	if hint.NodeID == nil || !bytes.Equal(
		hint.NodeID.SerializeCompressed(), peer,
	) || hint.ChannelID != quote.Scid || hint.CLTVExpiryDelta == 0 {

		return nil, fmt.Errorf("invoice receiving quote mismatch")
	}
	return invoice, nil
}
