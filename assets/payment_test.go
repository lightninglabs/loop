package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

func testOffer() swapkit.Offer {
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	offer := swapkit.Offer{
		Hash: sha256.Sum256([]byte("server preimage")),
		Terms: swapkit.Terms{
			PayoutAmount: 1000, MaxDebit: 1010, RefundHeight: 200,
			RecoveryDelta: 50, OfferExpiry: time.Unix(2000, 0),
		},
	}
	copy(offer.Terms.GroupKey[:], key.PubKey().SerializeCompressed())
	return offer
}

func testEvidence(t *testing.T, offer swapkit.Offer) (
	*lnrpc.Invoice, []*lnrpc.Channel) {

	t.Helper()
	rfq := sha256.Sum256([]byte("quote"))
	data := rfqmsg.JsonHtlc{RfqID: hex.EncodeToString(rfq[:])}
	data.Balances = []*rfqmsg.JsonAssetTranche{{
		AssetID: hex.EncodeToString(offer.Terms.GroupKey[1:]),
		Amount:  offer.Terms.PayoutAmount,
	}}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	channel, err := json.Marshal(rfqmsg.JsonAssetChannel{
		GroupKey: hex.EncodeToString(offer.Terms.GroupKey[:]),
	})
	require.NoError(t, err)
	return &lnrpc.Invoice{
		RHash: offer.Hash[:], State: lnrpc.Invoice_ACCEPTED,
		Htlcs: []*lnrpc.InvoiceHTLC{{
			State:  lnrpc.InvoiceHTLCState_ACCEPTED,
			ChanId: 42, HtlcIndex: 3, ExpiryHeight: 266,
			CustomChannelData: raw,
		}},
	}, []*lnrpc.Channel{{ChanId: 42, CustomChannelData: channel}}
}

// TestAccepted rejects native underpayments, BTC-only carriers, unrelated
// groups, partial/AMP payouts, and expiry windows lost to auto-cancellation.
func TestAccepted(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*lnrpc.Invoice, []*lnrpc.Channel)
	}{
		{"btc only", func(i *lnrpc.Invoice, _ []*lnrpc.Channel) {
			i.Htlcs[0].CustomChannelData = nil
		}},
		{"wrong hash", func(i *lnrpc.Invoice, _ []*lnrpc.Channel) {
			i.RHash = make([]byte, 32)
		}},
		{"unrelated group", func(_ *lnrpc.Invoice, c []*lnrpc.Channel) {
			c[0].CustomChannelData = []byte(`{"group_key":"wrong"}`)
		}},
		{"cancel boundary", func(i *lnrpc.Invoice, _ []*lnrpc.Channel) {
			i.Htlcs[0].ExpiryHeight = 265
		}},
		{"negative expiry", func(i *lnrpc.Invoice, _ []*lnrpc.Channel) {
			i.Htlcs[0].ExpiryHeight = -1
		}},
		{"AMP", func(i *lnrpc.Invoice, _ []*lnrpc.Channel) {
			i.Htlcs[0].Amp = &lnrpc.AMP{}
		}},
		{"multipart", func(i *lnrpc.Invoice, _ []*lnrpc.Channel) {
			i.Htlcs = append(i.Htlcs, i.Htlcs[0])
		}},
		{"underpayment", func(i *lnrpc.Invoice, _ []*lnrpc.Channel) {
			var d rfqmsg.JsonHtlc
			require.NoError(
				t,
				json.Unmarshal(
					i.Htlcs[0].CustomChannelData,
					&d,
				),
			)
			d.Balances[0].Amount--
			var err error
			i.Htlcs[0].CustomChannelData, err = json.Marshal(d)
			require.NoError(t, err)
		}},
		{"fake tranche", func(i *lnrpc.Invoice, _ []*lnrpc.Channel) {
			var d rfqmsg.JsonHtlc
			require.NoError(
				t,
				json.Unmarshal(
					i.Htlcs[0].CustomChannelData,
					&d,
				),
			)
			d.Balances[0].AssetID = hex.EncodeToString(
				make([]byte, 32),
			)
			var err error
			i.Htlcs[0].CustomChannelData, err = json.Marshal(d)
			require.NoError(t, err)
		}},
		{"overflow", func(i *lnrpc.Invoice, _ []*lnrpc.Channel) {
			var d rfqmsg.JsonHtlc
			require.NoError(
				t,
				json.Unmarshal(
					i.Htlcs[0].CustomChannelData,
					&d,
				),
			)
			d.Balances[0].Amount = math.MaxUint64
			d.Balances = append(d.Balances, d.Balances[0])
			var err error
			i.Htlcs[0].CustomChannelData, err = json.Marshal(d)
			require.NoError(t, err)
		}},
	}
	offer := testOffer()
	invoice, channels := testEvidence(t, offer)
	evidence, err := Accepted(offer, 15, invoice, channels, 100)
	require.NoError(t, err)
	require.Equal(t, offer.Terms.PayoutAmount, evidence.Amount)
	require.EqualValues(t, 266, evidence.ExpiryHeight)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invoice, channels := testEvidence(t, offer)
			test.mutate(invoice, channels)
			_, err := Accepted(offer, 15, invoice, channels, 100)
			require.ErrorIs(t, err, ErrEvidence)
		})
	}
}

func TestSettledUsesPersistedCircuitAfterChannelClose(t *testing.T) {
	offer := testOffer()
	offer.Terms.GroupKey[0] = 3
	invoice, channels := testEvidence(t, offer)
	evidence, err := Accepted(offer, 15, invoice, channels, 100)
	require.NoError(t, err)
	invoice.State = lnrpc.Invoice_SETTLED
	invoice.Htlcs[0].State = lnrpc.InvoiceHTLCState_SETTLED
	settled, err := Settled(offer, *evidence, invoice)
	require.NoError(t, err)
	require.True(t, settled)
	invoice.Htlcs[0].HtlcIndex++
	_, err = Settled(offer, *evidence, invoice)
	require.ErrorIs(t, err, ErrEvidence)
	invoice.Htlcs[0].HtlcIndex--
	evidence.GroupKey[0] = 2
	_, err = Settled(offer, *evidence, invoice)
	require.ErrorIs(t, err, ErrEvidence)
}
