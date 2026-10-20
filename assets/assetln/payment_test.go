package assetln

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func testTerms() Terms {
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	terms := Terms{
		Hash:   sha256.Sum256([]byte("server preimage")),
		Amount: 1000, MaxDebit: 1010, RefundHeight: 200,
		RecoveryBlocks: 50, HoldExpiryDelta: 15, Deadline: 2000,
	}
	copy(terms.GroupKey[:], key.PubKey().SerializeCompressed())
	return terms
}

func testEvidence(t *testing.T, terms Terms) (
	*lnrpc.Invoice, []*lnrpc.Channel) {

	t.Helper()
	rfq := sha256.Sum256([]byte("quote"))
	data := htlcData{RFQID: hex.EncodeToString(rfq[:])}
	data.Balances = append(data.Balances, struct {
		AssetID string `json:"asset_id"`
		Amount  uint64 `json:"amount"`
	}{hex.EncodeToString(terms.GroupKey[1:]), terms.Amount})
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	channel, err := json.Marshal(channelData{
		GroupKey: hex.EncodeToString(terms.GroupKey[:]),
	})
	require.NoError(t, err)
	return &lnrpc.Invoice{
		RHash: terms.Hash[:], State: lnrpc.Invoice_ACCEPTED,
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
			var d htlcData
			require.NoError(
				t,
				json.Unmarshal(
					i.Htlcs[0].CustomChannelData,
					&d,
				),
			)
			d.Balances[0].Amount--
			i.Htlcs[0].CustomChannelData, _ = json.Marshal(d)
		}},
		{"fake tranche", func(i *lnrpc.Invoice, _ []*lnrpc.Channel) {
			var d htlcData
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
			i.Htlcs[0].CustomChannelData, _ = json.Marshal(d)
		}},
		{"overflow", func(i *lnrpc.Invoice, _ []*lnrpc.Channel) {
			var d htlcData
			require.NoError(
				t,
				json.Unmarshal(
					i.Htlcs[0].CustomChannelData,
					&d,
				),
			)
			d.Balances[0].Amount = math.MaxUint64
			d.Balances = append(d.Balances, d.Balances[0])
			i.Htlcs[0].CustomChannelData, _ = json.Marshal(d)
		}},
	}
	terms := testTerms()
	invoice, channels := testEvidence(t, terms)
	evidence, err := Accepted(terms, invoice, channels, 100)
	require.NoError(t, err)
	require.Equal(t, terms.Amount, evidence.Amount)
	require.EqualValues(t, 266, evidence.ExpiryHeight)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invoice, channels := testEvidence(t, terms)
			test.mutate(invoice, channels)
			_, err := Accepted(terms, invoice, channels, 100)
			require.ErrorIs(t, err, ErrEvidence)
		})
	}
}

type mockLightning struct {
	lnrpc.LightningClient
	decoded  *lnrpc.PayReq
	channels []*lnrpc.Channel
}

func (m *mockLightning) DecodePayReq(context.Context, *lnrpc.PayReqString,
	...grpc.CallOption) (*lnrpc.PayReq, error) {

	return m.decoded, nil
}

func (m *mockLightning) ListChannels(
	context.Context,
	*lnrpc.ListChannelsRequest,
	...grpc.CallOption,
) (*lnrpc.ListChannelsResponse,
	error) {

	return &lnrpc.ListChannelsResponse{Channels: m.channels}, nil
}

type mockAssets struct {
	tapchannelrpc.TaprootAssetChannelsClient
	add func(
		*tapchannelrpc.AddInvoiceRequest,
	) *tapchannelrpc.AddInvoiceResponse
	send func(*tapchannelrpc.SendPaymentRequest)
}

func (m *mockAssets) AddInvoice(_ context.Context,
	r *tapchannelrpc.AddInvoiceRequest, _ ...grpc.CallOption) (
	*tapchannelrpc.AddInvoiceResponse, error) {

	return m.add(r), nil
}

func (m *mockAssets) SendPayment(_ context.Context,
	r *tapchannelrpc.SendPaymentRequest, _ ...grpc.CallOption) (
	tapchannelrpc.TaprootAssetChannels_SendPaymentClient, error) {

	m.send(r)
	return nil, nil
}

type mockRFQ struct {
	rfqrpc.RfqClient
	quote func(
		*rfqrpc.AddAssetSellOrderRequest,
	) *rfqrpc.PeerAcceptedSellQuote
}

func (m *mockRFQ) AddAssetSellOrder(_ context.Context,
	r *rfqrpc.AddAssetSellOrderRequest, _ ...grpc.CallOption) (
	*rfqrpc.AddAssetSellOrderResponse, error) {

	return &rfqrpc.AddAssetSellOrderResponse{
		Response: &rfqrpc.AddAssetSellOrderResponse_AcceptedQuote{
			AcceptedQuote: m.quote(r),
		},
	}, nil
}

func testDecoded(terms Terms) *lnrpc.PayReq {
	return &lnrpc.PayReq{
		PaymentHash: hex.EncodeToString(terms.Hash[:]),
		NumMsat:     1_000_000, Timestamp: 1000, Expiry: 1000,
		CltvExpiry: 166,
	}
}

// TestCreateHoldInvoice ensures no client preimage or BTC payout is requested,
// and detects older tapd versions silently dropping the final CLTV.
func TestCreateHoldInvoice(t *testing.T) {
	terms := testTerms()
	ln := &mockLightning{decoded: testDecoded(terms)}
	r := Receiver{
		Lightning: ln,
		Now:       func() time.Time { return time.Unix(1000, 0) },
		Assets: &mockAssets{add: func(
			req *tapchannelrpc.AddInvoiceRequest,
		) *tapchannelrpc.AddInvoiceResponse {

			require.Equal(
				t,
				terms.Hash[:],
				req.HodlInvoice.PaymentHash,
			)
			require.Equal(t, terms.GroupKey[:], req.GroupKey)
			require.Equal(t, terms.Amount, req.AssetAmount)
			require.Equal(t, terms.Amount, *req.AssetMinAmt)
			require.Empty(t, req.InvoiceRequest.RPreimage)
			require.Zero(t, req.InvoiceRequest.ValueMsat)
			require.Zero(t, req.InvoiceRequest.Value)
			require.EqualValues(
				t,
				166,
				req.InvoiceRequest.CltvExpiry,
			)
			return &tapchannelrpc.AddInvoiceResponse{
				InvoiceResult: &lnrpc.AddInvoiceResponse{
					PaymentRequest: "hold invoice",
				},
				AcceptedBuyQuote: &rfqrpc.PeerAcceptedBuyQuote{
					Id: make([]byte, 32), Expiry: 2000,
					AssetMaxAmount: terms.Amount,
					AssetSpec: &rfqrpc.AssetSpec{
						GroupPubKey: terms.GroupKey[1:],
					},
				},
			}
		}},
	}
	_, err := r.Create(t.Context(), terms, nil, 100)
	require.NoError(t, err)
	ln.decoded.CltvExpiry = 80
	_, err = r.Create(t.Context(), terms, nil, 100)
	require.ErrorIs(t, err, ErrEvidence)
}

// TestSourceAssetPayment verifies the sender cannot silently pay Bitcoin or
// negotiate a new quote beyond the persisted native asset debit cap.
func TestSourceAssetPayment(t *testing.T) {
	terms := testTerms()
	invoice, channels := testEvidence(t, terms)
	var rfq htlcData
	require.NoError(
		t,
		json.Unmarshal(invoice.Htlcs[0].CustomChannelData, &rfq),
	)
	id, err := hex.DecodeString(rfq.RFQID)
	require.NoError(t, err)
	quote := &rfqrpc.PeerAcceptedSellQuote{
		Id: id, AssetAmount: 1005, Expiry: 2000,
		AssetSpec: &rfqrpc.AssetSpec{GroupPubKey: terms.GroupKey[1:]},
	}
	sent := false
	s := Sender{
		Now: func() time.Time { return time.Unix(1000, 0) },
		Lightning: &mockLightning{
			decoded: testDecoded(terms), channels: channels,
		},
		RFQ: &mockRFQ{quote: func(
			req *rfqrpc.AddAssetSellOrderRequest,
		) *rfqrpc.PeerAcceptedSellQuote {

			require.Equal(
				t,
				terms.GroupKey[:],
				req.AssetSpecifier.GetGroupKey(),
			)
			require.EqualValues(t, 1_001_000, req.PaymentMaxAmt)
			require.Equal(t, req.PaymentMaxAmt, *req.PaymentMinAmt)
			require.NotNil(t, req.AssetRateLimit)
			require.Equal(
				t,
				rfqrpc.ExecutionPolicy_EXECUTION_POLICY_FOK,
				req.ExecutionPolicy,
			)
			return quote
		}},
		Assets: &mockAssets{
			send: func(req *tapchannelrpc.SendPaymentRequest) {
				sent = true
				require.Equal(
					t,
					terms.GroupKey[:],
					req.GroupKey,
				)
				require.Equal(t, quote.Id, req.RfqId)
				require.EqualValues(
					t,
					1,
					req.PaymentRequest.MaxParts,
				)
				require.Zero(t, req.AssetAmount)
			},
		},
	}
	plan, err := s.Quote(t.Context(), terms, "hold invoice", nil, 1000)
	require.NoError(t, err)
	_, err = s.Send(t.Context(), plan)
	require.NoError(t, err)
	require.True(t, sent)
	channelData := invoice.Htlcs[0].CustomChannelData
	payment := &lnrpc.Payment{
		PaymentHash: hex.EncodeToString(terms.Hash[:]),
		Status:      lnrpc.Payment_IN_FLIGHT,
		Htlcs: []*lnrpc.HTLCAttempt{{
			Status: lnrpc.HTLCAttempt_IN_FLIGHT,
			Route: &lnrpc.Route{
				Hops:              []*lnrpc.Hop{{ChanId: 42}},
				CustomChannelData: channelData,
			},
		}},
	}
	debit, err := s.Debit(t.Context(), plan, payment)
	require.NoError(t, err)
	require.Equal(t, terms.Amount, debit)
	quote.AssetAmount = terms.MaxDebit + 1
	_, err = s.Quote(t.Context(), terms, "hold invoice", nil, 1000)
	require.ErrorIs(t, err, ErrEvidence)
	payment.Htlcs[0].Route.CustomChannelData = nil
	_, err = s.Debit(t.Context(), plan, payment)
	require.ErrorIs(t, err, ErrEvidence)
}
