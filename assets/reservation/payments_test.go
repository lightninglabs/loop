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
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type paymentNode struct {
	lnrpc.LightningClient

	info    *lnrpc.GetInfoResponse
	balance int64
}

func (n *paymentNode) GetInfo(context.Context, *lnrpc.GetInfoRequest,
	...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {

	return n.info, nil
}

func (n *paymentNode) ListChannels(context.Context, *lnrpc.ListChannelsRequest,
	...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {

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

	probes                      []*routerrpc.RouteFeeRequest
	sends                       []*routerrpc.SendPaymentRequest
	result                      *lnrpc.Payment
	probeResult                 *routerrpc.RouteFeeResponse
	probeErr, trackErr, sendErr error
}

func (r *paymentRouter) EstimateRouteFee(_ context.Context,
	req *routerrpc.RouteFeeRequest,
	_ ...grpc.CallOption) (*routerrpc.RouteFeeResponse, error) {

	r.probes = append(r.probes, proto.Clone(req).(*routerrpc.RouteFeeRequest))
	return r.probeResult, r.probeErr
}

type paymentStream struct {
	grpc.ClientStream

	result *lnrpc.Payment
	err    error
}

func (s paymentStream) Recv() (*lnrpc.Payment, error) {
	return s.result, s.err
}

func (r *paymentRouter) TrackPaymentV2(context.Context,
	*routerrpc.TrackPaymentRequest,
	...grpc.CallOption) (routerrpc.Router_TrackPaymentV2Client, error) {

	return paymentStream{
		result: r.result,
		err:    r.trackErr,
	}, nil
}

func (r *paymentRouter) SendPaymentV2(_ context.Context,
	req *routerrpc.SendPaymentRequest,
	_ ...grpc.CallOption) (routerrpc.Router_SendPaymentV2Client, error) {

	r.sends = append(r.sends, proto.Clone(req).(*routerrpc.SendPaymentRequest))
	return paymentStream{
		result: r.result,
		err:    r.sendErr,
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
	r.Quote.ProbeInvoice = encode([32]byte{9}, r.Quote.EstimatedMainAmountMsat)
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
		info: &lnrpc.GetInfoResponse{
			IdentityPubkey: hex.EncodeToString(r.ClientKey.PubKey.SerializeCompressed()),
			SyncedToChain:  true,
			BlockHeight:    100,
		},
		balance: 100000,
	}
	router := &paymentRouter{
		probeResult: &routerrpc.RouteFeeResponse{
			RoutingFeeMsat: 10,
		},
		trackErr: status.Error(codes.NotFound, "no payment"),
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
	outcome, fee, err := p.Probe(t.Context(), r.Quote.ProbeInvoice)
	require.NoError(t, err)
	require.Equal(t, ProbeSucceeded, outcome)
	require.EqualValues(t, 10, fee)
	require.Empty(t, router.sends, "probes must never call SendPaymentV2")
	require.Len(t, router.probes, 1)
	require.Equal(t, r.Quote.ProbeInvoice, router.probes[0].PaymentRequest)
	require.Empty(t, router.probes[0].Dest, "use invoice-based probing")
	require.EqualValues(t, 10, router.probes[0].Timeout)
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

func TestProbeFailuresRemainVisible(t *testing.T) {
	p, r, _, router := newPaymentTest(t)
	for _, test := range []struct {
		err  error
		want ProbeOutcome
	}{
		{
			status.Error(codes.Unimplemented, "unsupported"),
			ProbeUnsupported,
		},
		{
			status.Error(codes.DeadlineExceeded, "timeout"),
			ProbeTimedOut,
		},
		{
			nil,
			ProbeFailed,
		},
	} {
		router.probeErr = test.err
		router.probeResult.FailureReason =
			lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE
		outcome, _, err := p.Probe(t.Context(), r.Quote.PrepayInvoice)
		require.NoError(t, err)
		require.Equal(t, test.want, outcome)
	}
}
