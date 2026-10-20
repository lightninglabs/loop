package test

import (
	"time"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

// CreateBlindedInvoice creates an encoded bLIP 39 invoice for client unit
// tests. Its encrypted payload is a placeholder and cannot forward a payment.
func CreateBlindedInvoice(params *chaincfg.Params, hash lntypes.Hash,
	amount btcutil.Amount) (string, error) {

	key, pub := CreateKey(42)
	features := lnwire.NewFeatureVector(lnwire.NewRawFeatureVector(
		lnwire.TLVOnionPayloadOptional, lnwire.PaymentAddrOptional,
		lnwire.MPPOptional, lnwire.RouteBlindingOptional,
		lnwire.Bolt11BlindedPathsRequired,
	), lnwire.Features)
	invoice, err := zpay32.NewInvoice(
		params, hash, time.Now(), zpay32.Description("blinded test"),
		zpay32.Amount(lnwire.NewMSatFromSatoshis(amount)),
		zpay32.Features(features),
		zpay32.WithBlindedPaymentPath(&zpay32.BlindedPaymentPath{
			CltvExpiryDelta:             40,
			HTLCMaxMsat:                 1_000_000_000,
			Features:                    lnwire.EmptyFeatureVector(),
			FirstEphemeralBlindingPoint: pub,
			Hops: []*sphinx.BlindedHopInfo{{
				BlindedNodePub: pub,
				CipherText:     make([]byte, 64),
			}},
		}),
	)
	if err != nil {
		return "", err
	}
	return invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			return ecdsa.SignCompact(key, chainhash.HashB(msg), true), nil
		},
	})
}
