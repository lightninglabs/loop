package reservation

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type paymentNode struct {
	lnrpc.LightningClient

	t       *testing.T
	info    *lnrpc.GetInfoResponse
	balance int64
	onInfo  func()
}

func (n *paymentNode) RawClientWithMacAuth(ctx context.Context) (
	context.Context, time.Duration, lnrpc.LightningClient) {

	return metadata.AppendToOutgoingContext(ctx, "macaroon", "node"),
		time.Minute, n
}

func (n *paymentNode) GetInfo(ctx context.Context, _ *lnrpc.GetInfoRequest,
	_ ...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {

	assertPaymentAuth(n.t, ctx, "node")
	if n.onInfo != nil {
		n.onInfo()
	}
	return n.info, nil
}

func (n *paymentNode) ListChannels(ctx context.Context,
	_ *lnrpc.ListChannelsRequest, _ ...grpc.CallOption) (
	*lnrpc.ListChannelsResponse, error) {

	assertPaymentAuth(n.t, ctx, "node")
	return &lnrpc.ListChannelsResponse{
		Channels: []*lnrpc.Channel{{
			Active:       true,
			LocalBalance: n.balance,
			LocalConstraints: &lnrpc.ChannelConstraints{
				ChanReserveSat: 1,
			},
		}},
	}, nil
}

type paymentRouter struct {
	routerrpc.RouterClient

	t *testing.T

	sends             []*routerrpc.SendPaymentRequest
	result            *lnrpc.Payment
	updates           []*lnrpc.Payment
	tracks            []*routerrpc.TrackPaymentRequest
	trackErr, sendErr error
}

func (r *paymentRouter) RawClientWithMacAuth(ctx context.Context) (
	context.Context, time.Duration, routerrpc.RouterClient) {

	return metadata.AppendToOutgoingContext(ctx, "macaroon", "router"),
		time.Minute, r
}

type paymentStream struct {
	grpc.ClientStream

	result  *lnrpc.Payment
	updates []*lnrpc.Payment
	err     error
}

func (s *paymentStream) Recv() (*lnrpc.Payment, error) {
	if len(s.updates) != 0 {
		update := s.updates[0]
		s.updates = s.updates[1:]
		return update, nil
	}
	return s.result, s.err
}

func (r *paymentRouter) TrackPaymentV2(ctx context.Context,
	req *routerrpc.TrackPaymentRequest,
	_ ...grpc.CallOption) (routerrpc.Router_TrackPaymentV2Client, error) {

	assertPaymentAuth(r.t, ctx, "router")
	r.tracks = append(r.tracks, proto.Clone(req).(*routerrpc.TrackPaymentRequest))
	return &paymentStream{
		updates: r.updates,
		result:  r.result,
		err:     r.trackErr,
	}, nil
}

func (r *paymentRouter) SendPaymentV2(ctx context.Context,
	req *routerrpc.SendPaymentRequest,
	_ ...grpc.CallOption) (routerrpc.Router_SendPaymentV2Client, error) {

	assertPaymentAuth(r.t, ctx, "router")
	r.sends = append(r.sends, proto.Clone(req).(*routerrpc.SendPaymentRequest))
	return &paymentStream{
		updates: r.updates,
		result:  r.result,
		err:     r.sendErr,
	}, nil
}

func newPaymentTest(t *testing.T) (*LndPayments, *Reservation,
	*paymentNode, *paymentRouter) {

	t.Helper()
	r := testReservation()
	r.Quote = testPurchaseQuote(r)
	key, _ := btcec.PrivKeyFromBytes([]byte{2})
	_, edge := btcec.PrivKeyFromBytes([]byte{3})
	now := time.Unix(1800000000, 0)
	id := rfqmsg.ID{1}
	q := &rfqrpc.PeerAcceptedBuyQuote{
		Id:   id[:],
		Scid: uint64(id.Scid()),
		Peer: hex.EncodeToString(edge.SerializeCompressed()),
		AssetSpec: &rfqrpc.AssetSpec{
			Id: r.AssetID[:],
		},
		AssetMaxAmount: 12,
		AskAssetRate: &rfqrpc.FixedPoint{
			Coefficient: "100000000",
		},
		Expiry: uint64(now.Add(2 * time.Hour).Unix()),
	}
	var err error
	r.Quote.PrepayRfq, err = proto.Marshal(q)
	require.NoError(t, err)
	encode := func(hash [32]byte, amount uint64) string {
		t.Helper()
		invoice, err := zpay32.NewInvoice(
			&chaincfg.RegressionNetParams, hash, now,
			zpay32.Amount(lnwire.MilliSatoshi(amount)),
			zpay32.Description("asset prepay test"), zpay32.Expiry(time.Hour),
			zpay32.CLTVExpiry(40), zpay32.PaymentAddr([32]byte{7}),
			zpay32.Features(lnwire.NewFeatureVector(lnwire.NewRawFeatureVector(
				lnwire.TLVOnionPayloadOptional, lnwire.PaymentAddrOptional,
			), lnwire.Features)),
			zpay32.RouteHint([]zpay32.HopHint{{
				NodeID:          edge,
				ChannelID:       q.Scid,
				CLTVExpiryDelta: 40,
			}}),
		)
		require.NoError(t, err)
		encoded, err := invoice.Encode(zpay32.MessageSigner{
			SignCompact: func(data []byte) ([]byte, error) {
				return ecdsa.SignCompact(key, chainhash.HashB(data), true), nil
			},
		})
		require.NoError(t, err)
		return encoded
	}
	r.Quote.PrepayInvoice = encode([32]byte{8}, r.Quote.PrepayAmountMsat)
	q.AssetMaxAmount = r.Amount + 1
	r.Quote.ProbeRfq, err = proto.Marshal(q)
	require.NoError(t, err)
	r.Quote.ProbeInvoice = encode(ProbeHash(r.ID), r.Quote.EstimatedMainAmountMsat)
	r.Quote.ExpiresAt = now.Add(10 * time.Minute).Unix()
	r.Terms, err = TermsFromRPC(r.Quote.Terms)
	require.NoError(t, err)
	r.Probes = ProbeResults{
		Main:      ProbeSucceeded,
		CheckedAt: now,
	}
	r.State = PayPrepay
	r.MaxRouteFeeMsat = 100
	node := &paymentNode{
		t: t,
		info: &lnrpc.GetInfoResponse{
			IdentityPubkey: hex.EncodeToString(r.ClientKey.PubKey.SerializeCompressed()),
			SyncedToChain:  true,
			BlockHeight:    100,
		},
		balance: 100000,
	}
	router := &paymentRouter{
		t:        t,
		trackErr: status.Error(codes.NotFound, "no payment"),
		result: &lnrpc.Payment{
			PaymentHash:   ProbeHash(r.ID).String(),
			Status:        lnrpc.Payment_FAILED,
			FailureReason: lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS,
		},
	}
	p, err := NewLndPayments(PaymentConfig{
		Node:           node,
		Router:         router,
		Params:         &chaincfg.RegressionNetParams,
		Now:            func() time.Time { return now },
		PaymentTimeout: time.Minute,
		ProbeTimeout:   10 * time.Second,
		CLTVLimit:      160,
		MinFinalCLTV:   40,
	})
	require.NoError(t, err)
	return p, r, node, router
}

func TestPaymentPreparationAndProbe(t *testing.T) {
	p, r, _, router := newPaymentTest(t)
	require.NoError(t, p.ValidateQuote(t.Context(), r.Quote))
	probe, err := p.PrepareProbe(t.Context(), r)
	require.NoError(t, err)
	r.ProbeNodeKey, r.ProbeRequest = probe.NodeKey, probe.Request
	r.ProbeDeadline = p.cfg.Now().Add(p.cfg.ProbeTimeout)
	require.Equal(t, ProbeHash(r.ID), probe.Hash)
	require.NoError(t, p.SendProbe(t.Context(), r, func(*lnrpc.Payment) error {
		return nil
	}))
	require.Len(t, router.sends, 1)
	require.EqualValues(t, 1, router.sends[0].MaxParts)
	router.sends = nil
	plan, err := p.Prepare(t.Context(), r)
	require.NoError(t, err)
	require.EqualValues(t, 1, plan.Request.MaxParts)
	require.EqualValues(t, r.MaxRouteFeeMsat, plan.Request.FeeLimitMsat)
	require.Equal(t, r.Quote.PrepayInvoice, plan.Request.PaymentRequest)
	router.sendErr = errors.New("lost reply")
	require.Error(t, p.Pay(t.Context(), plan.NodeKey, plan.Request))
	router.result = &lnrpc.Payment{
		PaymentHash: plan.Hash.String(),
		Status:      lnrpc.Payment_IN_FLIGHT,
	}
	router.trackErr = nil
	result, err := p.Lookup(t.Context(), plan.NodeKey, plan.Hash)
	require.NoError(t, err)
	require.Equal(t, lnrpc.Payment_IN_FLIGHT, result.Status)
	require.Len(t, router.sends, 1)
}

func TestProbeDispatchChecksDeadlineAfterNodeLookup(t *testing.T) {
	for _, elapsed := range []time.Duration{
		9 * time.Second, 10 * time.Second, 11 * time.Second,
	} {
		t.Run(elapsed.String(), func(t *testing.T) {
			p, r, node, router := newPaymentTest(t)
			now := p.cfg.Now()
			p.cfg.Now = func() time.Time { return now }
			plan, err := p.PrepareProbe(t.Context(), r)
			require.NoError(t, err)
			r.ProbeNodeKey, r.ProbeRequest = plan.NodeKey, plan.Request
			r.ProbeDeadline = now.Add(p.cfg.ProbeTimeout)
			node.onInfo = func() { now = now.Add(elapsed) }

			err = p.SendProbe(t.Context(), r, func(*lnrpc.Payment) error {
				return nil
			})
			if elapsed >= p.cfg.ProbeTimeout {
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Empty(t, router.sends)
			} else {
				require.NoError(t, err)
				require.Len(t, router.sends, 1)
			}
		})
	}
}

func TestProbeStreamsLiveRoutes(t *testing.T) {
	for _, recoverPayment := range []bool{false, true} {
		name := "send"
		if recoverPayment {
			name = "track"
		}
		t.Run(name, func(t *testing.T) {
			p, r, _, router := newPaymentTest(t)
			plan, err := p.PrepareProbe(t.Context(), r)
			require.NoError(t, err)
			r.ProbeNodeKey, r.ProbeRequest = plan.NodeKey, plan.Request
			r.ProbeDeadline = p.cfg.Now().Add(p.cfg.ProbeTimeout)
			router.trackErr = nil
			live := &lnrpc.Payment{
				PaymentHash: ProbeHash(r.ID).String(),
				Status:      lnrpc.Payment_IN_FLIGHT,
				Htlcs: []*lnrpc.HTLCAttempt{{
					Status: lnrpc.HTLCAttempt_IN_FLIGHT,
					Route:  &lnrpc.Route{TotalFeesMsat: 17},
				}},
			}
			router.updates = []*lnrpc.Payment{live}
			var updates []*lnrpc.Payment
			observe := func(update *lnrpc.Payment) error {
				updates = append(updates, update)
				return nil
			}
			if recoverPayment {
				err = p.TrackProbe(t.Context(), r, observe)
				require.Len(t, router.tracks, 1)
				require.False(t, router.tracks[0].NoInflightUpdates)
				require.Empty(t, router.sends)
			} else {
				err = p.SendProbe(t.Context(), r, observe)
				require.Len(t, router.sends, 1)
				require.False(t, router.sends[0].NoInflightUpdates)
			}
			require.NoError(t, err)
			require.Len(t, updates, 2)
			require.Same(t, live, updates[0])
			require.Equal(t, lnrpc.Payment_FAILED, updates[1].Status)
			require.Empty(t, updates[1].Htlcs)

			// A failed durable observation must stop the stream.
			writes := 0
			writeErr := errors.New("failed to save route")
			err = p.TrackProbe(t.Context(), r,
				func(*lnrpc.Payment) error {
					writes++
					return writeErr
				})
			require.ErrorIs(t, err, writeErr)
			require.Equal(t, 1, writes)
		})
	}
}

func TestPaymentLookupRequiresSavedNode(t *testing.T) {
	p, r, node, router := newPaymentTest(t)
	hash := lntypes.Hash{8}
	_, err := p.Lookup(t.Context(), r.ClientKey.PubKey, hash)
	require.ErrorIs(t, err, ErrPaymentNotFound)
	router.trackErr = status.Error(codes.Unavailable, "offline")
	_, err = p.Lookup(t.Context(), r.ClientKey.PubKey, hash)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPaymentNotFound)
	router.trackErr = status.Error(codes.NotFound, "other node")
	node.info.IdentityPubkey = hex.EncodeToString(r.Quote.ReceivingNodeKey)
	_, err = p.Lookup(t.Context(), r.ClientKey.PubKey, hash)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrPaymentNotFound)
}

func TestPaymentApprovalCannotBeBypassed(t *testing.T) {
	for _, test := range []string{"unapproved", "fee", "balance", "rfq", "hash", "expiry"} {
		t.Run(test, func(t *testing.T) {
			p, r, node, router := newPaymentTest(t)
			r.Probes.Main = ProbeFailed
			r.SkipProbe = true
			switch test {
			case "unapproved":
				r.State = AwaitApproval

			case "fee":
				r.MaxRouteFeeMsat = math.MaxUint64

			case "balance":
				node.balance = 1

			case "rfq":
				r.Quote.PrepayRfq = nil

			case "hash":
				r.Quote.ProbeInvoice = r.Quote.PrepayInvoice

			case "expiry":
				r.Quote.ExpiresAt = p.cfg.Now().Add(time.Minute).Unix()
			}
			_, err := p.Prepare(t.Context(), r)
			require.Error(t, err)
			require.Empty(t, router.sends)
		})
	}
}

func TestProbeRejectsUnboundInvoice(t *testing.T) {
	p, r, _, router := newPaymentTest(t)
	r.Quote.ReservationId[0] ^= 1
	_, err := p.PrepareProbe(t.Context(), r)
	require.Error(t, err)
	require.Empty(t, router.sends)
}

func assertPaymentAuth(t *testing.T, ctx context.Context, service string) {
	t.Helper()
	md, ok := metadata.FromOutgoingContext(ctx)
	require.True(t, ok)
	require.Equal(t, []string{service}, md.Get("macaroon"))
}

func TestQuoteRateConsistency(t *testing.T) {
	for _, tc := range []struct {
		name, prepay, probe     string
		prepayScale, probeScale uint32
		valid                   bool
	}{
		{"equal", "100", "100", 0, 0, true},
		{"scale", "1000", "100", 1, 0, true},
		{"boundary", "105", "100", 0, 0, true},
		{"over", "106", "100", 0, 0, false},
		{"expensive prepay", "1", "1000", 0, 0, false},
		{"expensive probe", "1000", "1", 0, 0, false},
		{"invalid", "0", "100", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compareQuoteRates(
				&rfqrpc.FixedPoint{Coefficient: tc.prepay, Scale: tc.prepayScale},
				&rfqrpc.FixedPoint{Coefficient: tc.probe, Scale: tc.probeScale},
			)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
