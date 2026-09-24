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
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/fn"
	"github.com/lightninglabs/taproot-assets/rfqmath"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type fixture struct {
	now     time.Time
	terms   InvoiceTerms
	receipt ReceiptTerms
	buy     *rfqrpc.PeerAcceptedBuyQuote
	peer    []byte
	encoded string
	invoice *lnrpc.Invoice
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
	f.receipt = ReceiptTerms{
		Hash:           f.terms.Hash,
		AssetID:        f.terms.AssetID,
		Amount:         1000,
		PaymentAddress: f.terms.PaymentAddress,
		AddIndex:       1,
		MaxParts:       2,
		ChannelID:      1,
	}
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
	f.invoice = &lnrpc.Invoice{
		RHash:        f.terms.Hash[:],
		ValueMsat:    1_000_000,
		State:        lnrpc.Invoice_ACCEPTED,
		CreationDate: f.now.Unix(),
		PaymentAddr:  f.terms.PaymentAddress[:],
		AddIndex:     1,
		Htlcs: []*lnrpc.InvoiceHTLC{{
			ChanId:       1,
			HtlcIndex:    1,
			ExpiryHeight: 180,
			State:        lnrpc.InvoiceHTLCState_ACCEPTED,
		}},
	}
	setBalance(t, f.invoice.Htlcs[0], f.terms.AssetID, id, 1000)
	return f
}

func setBalance(t *testing.T, part *lnrpc.InvoiceHTLC, assetID [32]byte,
	id rfqmsg.ID, amount uint64) {

	t.Helper()
	records, err := rfqmsg.NewHtlc([]*rfqmsg.AssetBalance{
		rfqmsg.NewAssetBalance(asset.ID(assetID), amount),
	}, fn.Some(id), fn.None[[]rfqmsg.ID]()).ToCustomRecords()
	require.NoError(t, err)
	part.CustomRecords = records
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

func TestExactIncomingReceipt(t *testing.T) {
	f := newFixture(t)
	result, err := ValidateIncoming(f.invoice, f.buy, f.receipt)
	require.NoError(t, err)
	require.EqualValues(t, 1000, result.AssetAmount)
	require.Equal(t, []uint32{180}, result.Expiries)
	for _, edit := range []func(*lnrpc.Invoice){
		func(v *lnrpc.Invoice) { v.Htlcs[0].CustomRecords = nil },
		func(v *lnrpc.Invoice) { v.Htlcs = append(v.Htlcs, v.Htlcs[0]) },
		func(v *lnrpc.Invoice) { v.Htlcs[0].ExpiryHeight = -1 },
		func(v *lnrpc.Invoice) { v.ValueMsat++ },
		func(v *lnrpc.Invoice) { v.Htlcs[0].ChanId++ },
		func(v *lnrpc.Invoice) { v.AddIndex++ },
		func(v *lnrpc.Invoice) { v.PaymentAddr[0]++ },
		func(v *lnrpc.Invoice) {
			v.Htlcs[0].State = lnrpc.InvoiceHTLCState_SETTLED
		},
	} {
		invoice := proto.Clone(f.invoice).(*lnrpc.Invoice)
		edit(invoice)
		_, err := ValidateIncoming(invoice, f.buy, f.receipt)
		require.Error(t, err)
	}
	var id rfqmsg.ID
	copy(id[:], f.buy.Id)
	for _, amount := range []uint64{999, 1001, math.MaxUint64} {
		invoice := proto.Clone(f.invoice).(*lnrpc.Invoice)
		setBalance(t, invoice.Htlcs[0], f.terms.AssetID, id, amount)
		_, err := ValidateIncoming(invoice, f.buy, f.receipt)
		require.Error(t, err, "no asset-unit rounding tolerance")
	}
	// Canceled parts neither count as money nor shorten settlement time.
	f.invoice.Htlcs = append(f.invoice.Htlcs, &lnrpc.InvoiceHTLC{
		State:        lnrpc.InvoiceHTLCState_CANCELED,
		ExpiryHeight: 1,
	})
	_, err = ValidateIncoming(f.invoice, f.buy, f.receipt)
	require.NoError(t, err)
	f.invoice.State = lnrpc.Invoice_SETTLED
	f.invoice.Htlcs[0].State = lnrpc.InvoiceHTLCState_SETTLED
	_, err = ValidateIncoming(f.invoice, f.buy, f.receipt)
	require.NoError(t, err, "old settled receipts remain collectible facts")
}

func TestIncomingPreservesEveryExpiry(t *testing.T) {
	f := newFixture(t)
	var id rfqmsg.ID
	copy(id[:], f.buy.Id)
	first := f.invoice.Htlcs[0]
	second := proto.Clone(first).(*lnrpc.InvoiceHTLC)
	second.HtlcIndex++
	second.ExpiryHeight--
	setBalance(t, first, f.terms.AssetID, id, 400)
	setBalance(t, second, f.terms.AssetID, id, 600)
	f.invoice.Htlcs = append(f.invoice.Htlcs, second)
	result, err := ValidateIncoming(f.invoice, f.buy, f.receipt)
	require.NoError(t, err)
	require.Equal(t, []uint32{180, 179}, result.Expiries)
	setBalance(t, second, f.terms.AssetID, id, 599)
	_, err = ValidateIncoming(f.invoice, f.buy, f.receipt)
	require.Error(t, err, "MPP rounding cannot reduce the principal")
}
