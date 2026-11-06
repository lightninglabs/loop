// Package payment checks asset quotes, invoices and actual Lightning receipts.
// Bitcoin routing amounts are millisatoshis; assets are integer base units.
package payment

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/taproot-assets/rfqmath"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrQuoteUnavailable means no usable quote exists. It does not prove
	// that an outstanding payment failed or that an invoice was canceled.
	ErrQuoteUnavailable = errors.New("asset payment quote unavailable")
)

// Rate decodes canonical positive decimal coefficients with bounded scale.
func Rate(value *rfqrpc.FixedPoint) (rfqmath.BigIntFixedPoint, error) {
	if value == nil || value.Scale > 18 || len(value.Coefficient) > 78 {
		return rfqmath.BigIntFixedPoint{}, fmt.Errorf("invalid asset rate")
	}
	n, ok := new(big.Int).SetString(value.Coefficient, 10)
	if !ok || n.Sign() <= 0 || n.String() != value.Coefficient {
		return rfqmath.BigIntFixedPoint{}, fmt.Errorf("invalid asset rate")
	}
	return rfqmath.BigIntFixedPoint{
		Coefficient: rfqmath.NewBigInt(n),
		Scale:       uint8(value.Scale),
	}, nil
}

// Units uses the deployed edge's millisatoshi-to-unit rounding.
func Units(msat uint64, rate rfqmath.BigIntFixedPoint) (uint64, error) {
	units := rfqmath.MilliSatoshiToUnits(lnwire.MilliSatoshi(msat), rate)
	result, ok := units.ScaleTo(0).ToUint64Checked()
	if !ok {
		return 0, fmt.Errorf("asset amount overflow")
	}
	return result, nil
}

// ExactMSat returns the least millisatoshi amount that delivers exactly the
// requested integer assets in one HTLC. Rounding an asset amount DOWN to msat
// and back can lose one asset unit. Round UP, then verify using the deployed
// conversion instead of granting a tolerance. Some rates cannot represent a
// given asset amount at millisatoshi resolution; reject those prices.
func ExactMSat(amount uint64, value *rfqrpc.FixedPoint) (uint64, error) {
	rate, err := Rate(value)
	if err != nil {
		return 0, err
	}
	if amount == 0 || amount > math.MaxInt64 {
		return 0, fmt.Errorf("invalid asset amount")
	}
	scale := new(big.Int).Exp(big.NewInt(10),
		new(big.Int).SetUint64(uint64(value.Scale)), nil)
	numerator := new(big.Int).Mul(new(big.Int).SetUint64(amount), scale)
	numerator.Mul(numerator, big.NewInt(100_000_000_000))
	coefficient, _ := new(big.Int).SetString(value.Coefficient, 10)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, coefficient, remainder)
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() || quotient.Sign() <= 0 {
		return 0, fmt.Errorf("converted invoice amount overflow")
	}
	msat := quotient.Uint64()
	actual, err := Units(msat, rate)
	if err != nil || actual != amount {
		return 0, fmt.Errorf("rate cannot deliver exact asset amount")
	}
	return msat, nil
}

func quoteIdentity(id, peer []byte, spec *rfqrpc.AssetSpec,
	expected [32]byte) error {

	if expected == ([32]byte{}) || len(id) != 32 ||
		bytes.Equal(id, make([]byte, 32)) || len(peer) != 33 ||
		spec == nil || !bytes.Equal(spec.Id, expected[:]) ||
		len(spec.GroupPubKey) != 0 {

		return fmt.Errorf("quote identity does not match asset contract")
	}
	key, err := btcec.ParsePubKey(peer)
	if err != nil || !bytes.Equal(key.SerializeCompressed(), peer) {
		return fmt.Errorf("noncanonical quote peer")
	}
	return nil
}

func parsePeer(value string) ([]byte, error) {
	peer, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(peer) != value {
		return nil, fmt.Errorf("invalid quote peer")
	}
	return peer, nil
}

// ReceivingAmount validates the receiving RFQ and returns the exact invoice
// amount. A quote's capacity is not an instruction to transfer that capacity.
// The edge floors its capacity's msat bound: a quote for A units may not permit
// the ceil-rounded payment for A. The caller may obtain capacity for A+1, but
// must keep the invoice, actual receipt and credit at A. Never negotiate a new
// rate implicitly while creating an invoice.
func ReceivingAmount(quote *rfqrpc.PeerAcceptedBuyQuote,
	assetID [32]byte, amount uint64, validThrough time.Time) (uint64, error) {

	if quote == nil {
		return 0, ErrQuoteUnavailable
	}
	peer, err := parsePeer(quote.Peer)
	if err != nil {
		return 0, err
	}
	if err := quoteIdentity(quote.Id, peer, quote.AssetSpec, assetID); err != nil {
		return 0, err
	}
	var id rfqmsg.ID
	copy(id[:], quote.Id)
	if quote.Scid != uint64(id.Scid()) || quote.Expiry > math.MaxInt64 ||
		!time.Unix(int64(quote.Expiry), 0).After(validThrough) {

		return 0, ErrQuoteUnavailable
	}
	capacity := quote.AssetMaxAmount
	if quote.AcceptedMaxAmount != 0 {
		capacity = min(capacity, quote.AcceptedMaxAmount)
	}
	if amount < quote.MinTransportableUnits || capacity < amount ||
		capacity > math.MaxInt64 {

		return 0, fmt.Errorf("receiving quote lacks required asset capacity")
	}
	msat, err := ExactMSat(amount, quote.AskAssetRate)
	if err != nil {
		return 0, err
	}
	rate, err := Rate(quote.AskAssetRate)
	if err != nil {
		return 0, err
	}
	maximum, err := rfqmath.UnitsToMilliSatoshi(
		rfqmath.NewBigIntFixedPoint(capacity, 0), rate,
	)
	if err != nil || uint64(maximum) < msat {
		return 0, fmt.Errorf("receiving quote lacks rounding capacity")
	}
	return msat, nil
}
