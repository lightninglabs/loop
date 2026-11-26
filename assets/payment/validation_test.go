package payment

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/taproot-assets/rfqmath"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type fixture struct {
	now     time.Time
	terms   InvoiceTerms
	buy     *rfqrpc.PeerAcceptedBuyQuote
	peer    []byte
	encoded string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	_, peer := btcec.PrivKeyFromBytes([]byte{2})
	f := fixture{
		now: time.Unix(1_800_000_000, 0),
	}
	f.terms = InvoiceTerms{
		AssetID:        [32]byte{7},
		Amount:         1000,
		MinFinalCLTV:   40,
		MinLifetime:    time.Minute,
		PaymentAddress: [32]byte{9},
	}
	preimage := [32]byte{8}
	f.terms.Hash = sha256.Sum256(preimage[:])
	copy(f.terms.Payee[:], key.PubKey().SerializeCompressed())
	var id rfqmsg.ID
	id[0], id[31] = 1, 2
	f.peer = peer.SerializeCompressed()
	f.buy = &rfqrpc.PeerAcceptedBuyQuote{
		Id:             id[:],
		Peer:           hex.EncodeToString(f.peer),
		Scid:           uint64(id.Scid()),
		AssetMaxAmount: 1000,
		AskAssetRate: &rfqrpc.FixedPoint{
			Coefficient: "100000000",
		},
		Expiry: uint64(f.now.Add(2 * time.Hour).Unix()),
		AssetSpec: &rfqrpc.AssetSpec{
			Id: f.terms.AssetID[:],
		},
	}
	inv, err := zpay32.NewInvoice(
		&chaincfg.RegressionNetParams, f.terms.Hash, f.now,
		zpay32.Amount(1_000_000), zpay32.Description("asset purchase"),
		zpay32.Expiry(time.Hour), zpay32.CLTVExpiry(80),
		zpay32.PaymentAddr(f.terms.PaymentAddress),
		zpay32.Features(lnwire.NewFeatureVector(lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadOptional, lnwire.PaymentAddrOptional,
		), lnwire.Features)),
		zpay32.RouteHint([]zpay32.HopHint{{
			NodeID:          peer,
			ChannelID:       f.buy.Scid,
			CLTVExpiryDelta: 40,
		}}),
	)
	require.NoError(t, err)
	f.encoded, err = inv.Encode(zpay32.MessageSigner{
		SignCompact: func(data []byte) ([]byte, error) {
			return ecdsa.SignCompact(key, chainhash.HashB(data), true), nil
		},
	})
	require.NoError(t, err)
	return f
}

func TestInvoiceBinding(t *testing.T) {
	f := newFixture(t)
	_, err := ValidateInvoice(f.encoded, f.buy, f.terms,
		&chaincfg.RegressionNetParams, f.now)
	require.NoError(t, err)
	for _, test := range []struct {
		name string
		edit func(*InvoiceTerms, *rfqrpc.PeerAcceptedBuyQuote)
	}{
		{
			"hash",
			func(v *InvoiceTerms, _ *rfqrpc.PeerAcceptedBuyQuote) {
				v.Hash[0]++
			},
		},
		{
			"payee",
			func(v *InvoiceTerms, _ *rfqrpc.PeerAcceptedBuyQuote) {
				v.Payee[1]++
			},
		},
		{
			"amount",
			func(v *InvoiceTerms, _ *rfqrpc.PeerAcceptedBuyQuote) {
				v.Amount--
			},
		},
		{
			"secret",
			func(v *InvoiceTerms, _ *rfqrpc.PeerAcceptedBuyQuote) {
				v.PaymentAddress[0]++
			},
		},
		{
			"cltv",
			func(v *InvoiceTerms, _ *rfqrpc.PeerAcceptedBuyQuote) {
				v.MinFinalCLTV = 81
			},
		},
		{
			"capacity",
			func(_ *InvoiceTerms, q *rfqrpc.PeerAcceptedBuyQuote) {
				q.AcceptedMaxAmount = 999
			},
		},
		{
			"expiry",
			func(_ *InvoiceTerms, q *rfqrpc.PeerAcceptedBuyQuote) {
				q.Expiry = uint64(f.now.Add(time.Minute).Unix())
			},
		},
		{
			"asset",
			func(_ *InvoiceTerms, q *rfqrpc.PeerAcceptedBuyQuote) {
				q.AssetSpec.Id[0]++
			},
		},
		{
			"hint",
			func(_ *InvoiceTerms, q *rfqrpc.PeerAcceptedBuyQuote) {
				q.Scid++
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			terms := f.terms
			quote := proto.Clone(f.buy).(*rfqrpc.PeerAcceptedBuyQuote)
			test.edit(&terms, quote)
			_, err := ValidateInvoice(f.encoded, quote, terms,
				&chaincfg.RegressionNetParams, f.now)
			require.Error(t, err)
		})
	}
	_, err = ValidateInvoice(f.encoded, f.buy, f.terms,
		&chaincfg.MainNetParams, f.now)
	require.Error(t, err)
}

func TestExactConversionAndCapacity(t *testing.T) {
	f := newFixture(t)
	f.buy.AskAssetRate.Coefficient = "300000000"
	rate, err := Rate(f.buy.AskAssetRate)
	require.NoError(t, err)
	floor, err := rfqmath.UnitsToMilliSatoshi(
		rfqmath.NewBigIntFixedPoint(1000, 0), rate,
	)
	require.NoError(t, err)
	units, err := Units(uint64(floor), rate)
	require.NoError(t, err)
	require.EqualValues(t, 999, units)
	msat, err := ExactMSat(1000, f.buy.AskAssetRate)
	require.NoError(t, err)
	require.EqualValues(t, 333_334, msat)
	units, err = Units(msat, rate)
	require.NoError(t, err)
	require.EqualValues(t, 1000, units)
	_, err = ReceivingAmount(f.buy, f.terms.AssetID, 1000, f.now)
	require.ErrorContains(t, err, "rounding capacity")
	f.buy.AssetMaxAmount = 1001
	actual, err := ReceivingAmount(f.buy, f.terms.AssetID, 1000, f.now)
	require.NoError(t, err)
	require.Equal(t, msat, actual)
	f.buy.AcceptedMaxAmount = 1000
	_, err = ReceivingAmount(f.buy, f.terms.AssetID, 1000, f.now)
	require.ErrorContains(t, err, "rounding capacity")

	// More than one asset unit per msat can make an amount unrepresentable.
	_, err = ExactMSat(1, &rfqrpc.FixedPoint{
		Coefficient: "200000000000",
	})
	require.ErrorContains(t, err, "exact asset amount")
	_, err = ExactMSat(math.MaxInt64, &rfqrpc.FixedPoint{
		Coefficient: "1",
	})
	require.ErrorContains(t, err, "overflow")
	for _, coefficient := range []string{
		"", "0", "-1", "+1", "01", "1.5", strings.Repeat("9", 79),
	} {
		_, err := Rate(&rfqrpc.FixedPoint{
			Coefficient: coefficient,
		})
		require.Error(t, err)
	}
	_, err = Rate(&rfqrpc.FixedPoint{
		Coefficient: "1",
		Scale:       19,
	})
	require.Error(t, err)
}
