package payment

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

// TestRequestFromInvoice checks that a signed invoice's supported semantics
// are copied into a component-based payment request.
func TestRequestFromInvoice(t *testing.T) {
	t.Parallel()

	now := time.Unix(123456789, 0)
	paymentHash := [32]byte{1, 2, 3}
	paymentAddr := [32]byte{4, 5, 6}
	privateKey, destination := btcec.PrivKeyFromBytes([]byte{7, 8, 9})
	_, hintNode := btcec.PrivKeyFromBytes([]byte{10, 11, 12})
	routeHints := [][]zpay32.HopHint{{{
		NodeID:                    hintNode,
		ChannelID:                 123,
		FeeBaseMSat:               456,
		FeeProportionalMillionths: 789,
		CLTVExpiryDelta:           40,
	}}}
	features := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
			lnwire.PaymentAddrRequired,
			lnwire.MPPOptional,
			lnwire.RouteBlindingOptional,
		),
		lnwire.Features,
	)
	invoice, err := zpay32.NewInvoice(
		&chaincfg.TestNet3Params, paymentHash, now,
		zpay32.Description("test"),
		zpay32.Amount(123456),
		zpay32.Destination(destination),
		zpay32.PaymentAddr(paymentAddr),
		zpay32.CLTVExpiry(144),
		zpay32.RouteHint(routeHints[0]),
		zpay32.Features(features),
	)
	require.NoError(t, err)

	encoded := encodeInvoice(t, invoice, privateKey)
	request, err := RequestFromInvoice(
		&chaincfg.TestNet3Params, encoded, now.Add(time.Minute),
	)
	require.NoError(t, err)

	require.Empty(t, request.Invoice)
	require.Equal(t, route.NewVertex(destination), request.Target)
	require.Equal(t, lnwire.MilliSatoshi(123456), request.AmountMsat)
	require.Zero(t, request.Amount)
	require.Equal(t, lntypes.Hash(paymentHash), *request.PaymentHash)
	require.Equal(t, paymentAddr, *request.PaymentAddr)
	require.Equal(t, uint16(144), request.FinalCLTVDelta)
	require.Equal(t, routeHints, request.RouteHints)
	require.Equal(t, []lnrpc.FeatureBit{
		lnrpc.FeatureBit_TLV_ONION_REQ,
		lnrpc.FeatureBit_PAYMENT_ADDR_REQ,
		lnrpc.FeatureBit_MPP_OPT,
		lnrpc.FeatureBit_ROUTE_BLINDING_OPTIONAL,
	}, request.DestFeatures)
}

// TestRequestFromInvoiceRejectsTampering checks that decoding and integrity
// validation are part of constructing a payment request.
func TestRequestFromInvoiceRejectsTampering(t *testing.T) {
	t.Parallel()

	now := time.Unix(123456789, 0)
	paymentHash := [32]byte{1, 2, 3}
	privateKey, _ := btcec.PrivKeyFromBytes([]byte{7, 8, 9})
	invoice, err := zpay32.NewInvoice(
		&chaincfg.TestNet3Params, paymentHash, now,
		zpay32.Description("test"), zpay32.Amount(123456),
	)
	require.NoError(t, err)

	encoded := encodeInvoice(t, invoice, privateKey)
	replacement := byte('q')
	if encoded[len(encoded)-1] == replacement {
		replacement = 'p'
	}
	encoded = encoded[:len(encoded)-1] + string(replacement)

	_, err = RequestFromInvoice(
		&chaincfg.TestNet3Params, encoded, now.Add(time.Minute),
	)
	require.ErrorContains(t, err, "decode invoice")
}

// TestRequestFromDecodedInvoiceRejectsUnsupported checks that invoice
// semantics which cannot be represented safely are rejected.
func TestRequestFromDecodedInvoiceRejectsUnsupported(t *testing.T) {
	t.Parallel()

	now := time.Unix(123456789, 0)
	newInvoice := func(t *testing.T) *zpay32.Invoice {
		t.Helper()

		paymentHash := [32]byte{1, 2, 3}
		_, destination := btcec.PrivKeyFromBytes([]byte{7, 8, 9})
		invoice, err := zpay32.NewInvoice(
			&chaincfg.TestNet3Params, paymentHash, now,
			zpay32.Description("test"),
			zpay32.Amount(123456),
			zpay32.Destination(destination),
			zpay32.Expiry(time.Hour),
		)
		require.NoError(t, err)

		return invoice
	}

	testCases := []struct {
		name   string
		mutate func(*zpay32.Invoice)
		now    time.Time
		err    string
	}{
		{
			name: "expired",
			now:  now.Add(time.Hour + time.Second),
			err:  "invoice is expired",
		},
		{
			name: "metadata",
			mutate: func(invoice *zpay32.Invoice) {
				invoice.Metadata = []byte{}
			},
			err: "invoice metadata is not supported",
		},
		{
			name: "blinded payment path",
			mutate: func(invoice *zpay32.Invoice) {
				invoice.BlindedPaymentPaths =
					[]*zpay32.BlindedPaymentPath{{}}
			},
			err: "blinded payment paths are not supported",
		},
		{
			name: "AMP",
			mutate: func(invoice *zpay32.Invoice) {
				invoice.Features = lnwire.NewFeatureVector(
					lnwire.NewRawFeatureVector(
						lnwire.AMPOptional,
					),
					lnwire.Features,
				)
			},
			err: "AMP invoices are not supported",
		},
		{
			name: "unknown feature",
			mutate: func(invoice *zpay32.Invoice) {
				invoice.Features = lnwire.NewFeatureVector(
					lnwire.NewRawFeatureVector(999),
					lnwire.Features,
				)
			},
			err: "invoice feature bit 999 is not supported",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			invoice := newInvoice(t)
			if tc.mutate != nil {
				tc.mutate(invoice)
			}

			checkTime := now.Add(time.Minute)
			if !tc.now.IsZero() {
				checkTime = tc.now
			}

			_, err := requestFromDecodedInvoice(invoice, checkTime)
			require.ErrorContains(t, err, tc.err)
		})
	}
}

// encodeInvoice signs and encodes an invoice for testing.
func encodeInvoice(t *testing.T, invoice *zpay32.Invoice,
	privateKey *btcec.PrivateKey) string {

	t.Helper()

	encoded, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(message []byte) ([]byte, error) {
			hash := chainhash.HashB(message)

			return ecdsa.SignCompact(privateKey, hash, true), nil
		},
	})
	require.NoError(t, err)

	return encoded
}
