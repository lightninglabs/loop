// Package payment provides Loop-specific construction and validation of
// outgoing Lightning payments.
package payment

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

// RequestFromInvoice decodes and verifies an encoded BOLT 11 invoice, then
// builds a component-based payment request from its supported fields.
func RequestFromInvoice(chainParams *chaincfg.Params, encoded string,
	now time.Time) (lndclient.SendPaymentRequest, error) {

	invoice, err := zpay32.Decode(encoded, chainParams)
	if err != nil {
		return lndclient.SendPaymentRequest{},
			fmt.Errorf("decode invoice: %w", err)
	}

	request, err := requestFromDecodedInvoice(invoice, now)
	if err != nil {
		return lndclient.SendPaymentRequest{},
			fmt.Errorf("invalid invoice: %w", err)
	}

	return request, nil
}

// requestFromDecodedInvoice builds an allowlisted component payment request
// from a decoded invoice.
func requestFromDecodedInvoice(invoice *zpay32.Invoice,
	now time.Time) (lndclient.SendPaymentRequest, error) {

	if invoice == nil {
		return lndclient.SendPaymentRequest{}, errors.New("invoice is nil")
	}

	if invoice.Metadata != nil {
		return lndclient.SendPaymentRequest{}, errors.New(
			"invoice metadata is not supported",
		)
	}

	if len(invoice.BlindedPaymentPaths) != 0 {
		return lndclient.SendPaymentRequest{}, errors.New(
			"blinded payment paths are not supported",
		)
	}

	if invoice.Features == nil {
		return lndclient.SendPaymentRequest{}, errors.New(
			"invoice features are missing",
		)
	}

	if invoice.Features.HasFeature(lnwire.AMPOptional) {
		return lndclient.SendPaymentRequest{}, errors.New(
			"AMP invoices are not supported",
		)
	}

	if now.After(invoice.Timestamp.Add(invoice.Expiry())) {
		return lndclient.SendPaymentRequest{}, errors.New(
			"invoice is expired",
		)
	}

	if invoice.MilliSat == nil || *invoice.MilliSat <= 0 {
		return lndclient.SendPaymentRequest{}, errors.New(
			"invoice amount must be greater than zero",
		)
	}

	if invoice.PaymentHash == nil {
		return lndclient.SendPaymentRequest{}, errors.New(
			"invoice payment hash is missing",
		)
	}

	if invoice.Destination == nil {
		return lndclient.SendPaymentRequest{}, errors.New(
			"invoice destination is missing",
		)
	}

	finalCltvDelta := invoice.MinFinalCLTVExpiry()
	if finalCltvDelta > math.MaxUint16 {
		return lndclient.SendPaymentRequest{}, fmt.Errorf(
			"invoice final CLTV delta %d exceeds maximum %d",
			finalCltvDelta, uint64(math.MaxUint16),
		)
	}

	destFeatures := make(
		[]lnrpc.FeatureBit, 0, len(invoice.Features.Features()),
	)
	for feature := range invoice.Features.Features() {
		if !supportedInvoiceFeature(feature) {
			return lndclient.SendPaymentRequest{}, fmt.Errorf(
				"invoice feature bit %d is not supported", feature,
			)
		}

		destFeatures = append(destFeatures, lnrpc.FeatureBit(feature))
	}
	sort.Slice(destFeatures, func(i, j int) bool {
		return destFeatures[i] < destFeatures[j]
	})

	paymentHash := lntypes.Hash(*invoice.PaymentHash)
	request := lndclient.SendPaymentRequest{
		Target:         route.NewVertex(invoice.Destination),
		AmountMsat:     *invoice.MilliSat,
		PaymentHash:    &paymentHash,
		FinalCLTVDelta: uint16(finalCltvDelta),
		RouteHints:     invoice.RouteHints,
		DestFeatures:   destFeatures,
	}

	invoice.PaymentAddr.WhenSome(func(addr [32]byte) {
		request.PaymentAddr = &addr
	})

	return request, nil
}

// supportedInvoiceFeature returns true for invoice feature bits that are
// represented by a component-based SendPayment request.
func supportedInvoiceFeature(feature lnwire.FeatureBit) bool {
	switch feature {
	case lnwire.TLVOnionPayloadRequired,
		lnwire.TLVOnionPayloadOptional,
		lnwire.PaymentAddrRequired,
		lnwire.PaymentAddrOptional,
		lnwire.MPPRequired,
		lnwire.MPPOptional,
		lnwire.RouteBlindingRequired,
		lnwire.RouteBlindingOptional:

		return true

	default:
		return false
	}
}
