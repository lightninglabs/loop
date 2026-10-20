package assetln

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
)

// Sender prepares native asset debits through tapd. All connections must refer
// to the same authenticated local LND/tapd pair.
type Sender struct {
	Assets    tapchannelrpc.TaprootAssetChannelsClient
	RFQ       rfqrpc.RfqClient
	Lightning lnrpc.LightningClient
	Router    routerrpc.RouterClient
	Now       func() time.Time
}

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// PaymentPlan is persisted before Send. The same hash and quote are used after
// restart; a timed-out RPC must be resolved by Track, never by a fresh payment.
type PaymentPlan struct {
	Terms        Terms
	Invoice      string
	Quote        *rfqrpc.PeerAcceptedSellQuote
	FeeLimitMSat int64
	InvoiceMSat  int64
	Peer         []byte
}

// Quote negotiates only after deposit risk acceptance. A native-unit rate cap
// bounds the whole payment including routing fees, and a single-part policy
// avoids accepting partial asset payouts in the initial protocol.
func (s *Sender) Quote(ctx context.Context, terms Terms, invoice string,
	peer []byte, feeLimitMSat int64) (*PaymentPlan, error) {

	now := s.now()
	if err := terms.Validate(now); err != nil {
		return nil, err
	}
	decoded, err := s.Lightning.DecodePayReq(ctx, &lnrpc.PayReqString{
		PayReq: invoice,
	})
	if err != nil {
		return nil, err
	}
	if err := validateInvoice(terms, decoded, now); err != nil {
		return nil, err
	}
	if feeLimitMSat < 0 || decoded.NumMsat > math.MaxInt64-feeLimitMSat {
		return nil, fmt.Errorf("invalid asset payment fee limit")
	}
	maxMSat := uint64(decoded.NumMsat + feeLimitMSat)
	// Native conversion floors the product. The greatest rate whose
	// product is strictly below MaxDebit+1 still respects the native cap.
	const scale = 11
	ceiling := assetRateCeiling(terms.MaxDebit, maxMSat, scale)
	seconds := terms.Deadline - now.Unix()
	if seconds > 60 {
		seconds = 60
	}
	policy := rfqrpc.ExecutionPolicy_EXECUTION_POLICY_FOK
	response, err := s.RFQ.AddAssetSellOrder(ctx,
		&rfqrpc.AddAssetSellOrderRequest{
			AssetSpecifier: &rfqrpc.AssetSpecifier{
				Id: &rfqrpc.AssetSpecifier_GroupKey{
					GroupKey: terms.GroupKey[:],
				},
			},
			PeerPubKey: peer, PaymentMaxAmt: maxMSat,
			PaymentMinAmt: &maxMSat, Expiry: uint64(terms.Deadline),
			TimeoutSeconds:  uint32(seconds),
			ExecutionPolicy: policy,
			AssetRateLimit: &rfqrpc.FixedPoint{
				Coefficient: ceiling.String(), Scale: scale,
			},
		})
	if err != nil {
		return nil, err
	}
	plan := &PaymentPlan{
		Terms:        terms,
		Invoice:      invoice,
		Quote:        response.GetAcceptedQuote(),
		FeeLimitMSat: feeLimitMSat, InvoiceMSat: decoded.NumMsat,
		Peer: bytes.Clone(peer),
	}
	if err := plan.Validate(s.now()); err != nil {
		return nil, err
	}
	return plan, nil
}

// Validate checks persisted RFQ identity, expiry, fill, and native debit cap.
func (p *PaymentPlan) Validate(now time.Time) error {
	if p == nil {
		return ErrEvidence
	}
	if err := p.Terms.Validate(now); err != nil {
		return err
	}
	quote := p.Quote
	if p.Invoice == "" || p.InvoiceMSat <= 0 || p.FeeLimitMSat < 0 ||
		p.InvoiceMSat > math.MaxInt64-p.FeeLimitMSat || quote == nil ||
		quote.AssetSpec == nil || len(quote.AssetSpec.Id) != 0 ||
		!bytes.Equal(
			quote.AssetSpec.GroupPubKey,
			p.Terms.GroupKey[1:],
		) ||
		len(quote.Id) != 32 || quote.AssetAmount == 0 ||
		quote.AssetAmount > p.Terms.MaxDebit ||
		quote.Expiry < uint64(p.Terms.Deadline) ||
		quote.Expiry <= uint64(now.Unix()) ||
		(len(p.Peer) != 0 && quote.Peer != hex.EncodeToString(p.Peer)) {

		return ErrEvidence
	}
	maxMSat := uint64(p.InvoiceMSat + p.FeeLimitMSat)
	if quote.AcceptedMaxAmount != 0 && quote.AcceptedMaxAmount < maxMSat {
		return ErrEvidence
	}
	return nil
}

// Send dispatches the persisted plan without disclosing the preimage. Losing
// this stream is ambiguous; Track must establish the payment's final outcome.
func (s *Sender) Send(ctx context.Context, plan *PaymentPlan) (
	tapchannelrpc.TaprootAssetChannels_SendPaymentClient, error) {

	if err := plan.Validate(s.now()); err != nil {
		return nil, err
	}
	timeout := plan.Terms.Deadline - s.now().Unix()
	if timeout <= 0 || timeout > math.MaxInt32 {
		return nil, ErrEvidence
	}
	return s.Assets.SendPayment(ctx, &tapchannelrpc.SendPaymentRequest{
		GroupKey: plan.Terms.GroupKey[:], RfqId: plan.Quote.Id,
		PeerPubkey: plan.Peer,
		PaymentRequest: &routerrpc.SendPaymentRequest{
			PaymentRequest: plan.Invoice,
			FeeLimitMsat:   plan.FeeLimitMSat,
			TimeoutSeconds: int32(timeout), MaxParts: 1,
		},
	})
}

// Track resumes a payment by hash, including after a lost dispatch response.
func (s *Sender) Track(ctx context.Context, hash [32]byte) (
	routerrpc.Router_TrackPaymentV2Client, error) {

	return s.Router.TrackPaymentV2(ctx, &routerrpc.TrackPaymentRequest{
		PaymentHash: hash[:], NoInflightUpdates: false,
	})
}

// Debit verifies the actual nonfailed source HTLC's native debit and RFQ on
// the local asset channel. IN_FLIGHT alone cannot commit execution.
func (s *Sender) Debit(ctx context.Context, plan *PaymentPlan,
	payment *lnrpc.Payment) (uint64, error) {

	if plan == nil || plan.Quote == nil || payment == nil ||
		payment.PaymentHash != hex.EncodeToString(plan.Terms.Hash[:]) ||
		(payment.Status != lnrpc.Payment_IN_FLIGHT &&
			payment.Status != lnrpc.Payment_SUCCEEDED) {

		return 0, ErrEvidence
	}
	var live *lnrpc.HTLCAttempt
	for _, attempt := range payment.Htlcs {
		if attempt == nil ||
			attempt.Status == lnrpc.HTLCAttempt_FAILED {
			continue
		}
		if live != nil || attempt.Route == nil ||
			len(
				attempt.Route.Hops,
			) == 0 || attempt.Route.Hops[0] == nil {

			return 0, ErrEvidence
		}
		live = attempt
	}
	if live == nil {
		return 0, ErrEvidence
	}
	channels, err := s.Lightning.ListChannels(
		ctx,
		&lnrpc.ListChannelsRequest{},
	)
	if err != nil {
		return 0, err
	}
	if channels == nil {
		return 0, ErrEvidence
	}
	for _, channel := range channels.Channels {
		if channel == nil ||
			channel.ChanId != live.Route.Hops[0].ChanId {
			continue
		}
		amount, rfq, err := nativeAmount(
			plan.Terms.GroupKey, live.Route.CustomChannelData,
			channel.CustomChannelData,
		)
		if err != nil || amount > plan.Terms.MaxDebit ||
			!bytes.Equal(rfq[:], plan.Quote.Id) {

			return 0, ErrEvidence
		}
		return amount, nil
	}
	return 0, ErrEvidence
}

// assetRateCeiling returns the greatest fixed-point coefficient whose floored
// conversion at maxMSat cannot exceed maxDebit. Big integers preserve the
// MaxUint64 boundary and the strict inequality before integer division.
func assetRateCeiling(maxDebit, maxMSat uint64, scale int64) *big.Int {
	precision := new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil)
	ceiling := new(big.Int).SetUint64(maxDebit)
	ceiling.Add(ceiling, big.NewInt(1))
	ceiling.Mul(ceiling, big.NewInt(100_000_000_000))
	ceiling.Mul(ceiling, precision)
	ceiling.Sub(ceiling, big.NewInt(1))
	return ceiling.Div(ceiling, new(big.Int).SetUint64(maxMSat))
}
