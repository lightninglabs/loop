package reservation

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop/assets/payment"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// PaymentConfig supplies the client's own LND. Prepays use BTC channels; the
// server's independent edge delivers the quoted asset fee.
type PaymentConfig struct {
	Node           lnrpc.LightningClient
	Router         routerrpc.RouterClient
	Params         *chaincfg.Params
	Now            func() time.Time
	PaymentTimeout time.Duration
	ProbeTimeout   time.Duration
	CLTVLimit      uint32
	MinFinalCLTV   uint32
}

// LndPayments validates, probes, and pays without owning purchase state.
// The FSM saves the returned request before dispatch and reconciles its hash.
type LndPayments struct {
	cfg PaymentConfig

	// probeSlot allows one reservation probe RPC at a time per adapter.
	// A send acquires the slot; a receive releases it. Waiting for the slot
	// uses the probe's timeout and can be canceled.
	probeSlot chan struct{}
}

// NewLndPayments requires explicit timing caps. It does not change the user's
// approved routing fee or exchange-price limit.
func NewLndPayments(cfg PaymentConfig) (*LndPayments, error) {
	if cfg.Node == nil || cfg.Router == nil || cfg.Params == nil ||
		cfg.Now == nil || cfg.PaymentTimeout <= 0 ||
		cfg.PaymentTimeout > time.Minute ||
		cfg.PaymentTimeout%time.Second != 0 ||
		cfg.ProbeTimeout <= 0 || cfg.ProbeTimeout > time.Minute ||
		cfg.ProbeTimeout%time.Second != 0 ||
		cfg.MinFinalCLTV == 0 || cfg.MinFinalCLTV > math.MaxUint16 ||
		cfg.CLTVLimit == 0 || cfg.CLTVLimit > math.MaxInt32 {

		return nil, errors.New("invalid reservation payment configuration")
	}
	return &LndPayments{
		cfg:       cfg,
		probeSlot: make(chan struct{}, 1),
	}, nil
}

// ValidateQuote binds the prepay's signed BTC invoice to the receiving RFQ.
// The main invoice only supplies an estimate and a route to the same edge.
func (p *LndPayments) ValidateQuote(_ context.Context,
	q *swapserverrpc.AssetReservationQuote) error {

	if q == nil || len(q.PrepayRfq) == 0 || len(q.PrepayRfq) > 1<<20 ||
		len(q.ReceivingNodeKey) != 33 || len(q.EdgeKey) != 33 {

		return errors.New("missing receiving quote")
	}
	terms, err := TermsFromRPC(q.Terms)
	if err != nil {
		return err
	}
	var rfq rfqrpc.PeerAcceptedBuyQuote
	if err := proto.Unmarshal(q.PrepayRfq, &rfq); err != nil {
		return err
	}
	prepay, err := p.decode(q.PrepayInvoice)
	if err != nil {
		return err
	}
	_, err = payment.ValidateInvoice(q.PrepayInvoice, &rfq,
		payment.InvoiceTerms{
			AssetID:      terms.AssetID,
			Amount:       terms.Fee,
			Hash:         *prepay.PaymentHash,
			Payee:        [33]byte(q.ReceivingNodeKey),
			MinFinalCLTV: uint64(p.cfg.MinFinalCLTV),
			MinLifetime:  p.cfg.PaymentTimeout + 30*time.Second,
		}, p.cfg.Params, p.cfg.Now(),
	)
	if err != nil {
		return err
	}
	main, err := p.decode(q.ProbeInvoice)
	if err != nil {
		return err
	}
	if *main.PaymentHash == *prepay.PaymentHash ||
		rfq.Peer != hex.EncodeToString(q.EdgeKey) ||
		uint64(*prepay.MilliSat) != q.PrepayAmountMsat ||
		uint64(*main.MilliSat) != q.EstimatedMainAmountMsat {

		return errors.New("quote invoice amount or identity mismatch")
	}
	validUntil := p.cfg.Now().Add(p.cfg.PaymentTimeout + 30*time.Second)
	if !time.Unix(q.ExpiresAt, 0).After(validUntil) {
		return errors.New("quote lacks time for payment")
	}
	for _, invoice := range []*zpay32.Invoice{prepay, main} {
		if !bytes.Equal(invoice.Destination.SerializeCompressed(),
			q.ReceivingNodeKey) ||
			len(invoice.RouteHints) != 1 ||
			len(invoice.RouteHints[0]) != 1 ||
			len(invoice.BlindedPaymentPaths) != 0 ||
			!bytes.Equal(invoice.RouteHints[0][0].NodeID.SerializeCompressed(),
				q.EdgeKey) ||
			q.ExpiresAt > invoice.Timestamp.Add(invoice.Expiry()).Unix() {

			return errors.New("quote changed invoice edge or expiry")
		}
	}
	return nil
}

func (p *LndPayments) decode(encoded string) (*zpay32.Invoice, error) {
	if len(encoded) == 0 || len(encoded) > 16*1024 {
		return nil, errors.New("invalid payment invoice size")
	}
	invoice, err := zpay32.Decode(encoded, p.cfg.Params)
	if err != nil {
		return nil, err
	}
	if invoice.PaymentHash == nil || *invoice.PaymentHash == ([32]byte{}) ||
		invoice.Destination == nil || invoice.MilliSat == nil ||
		*invoice.MilliSat <= 0 || uint64(*invoice.MilliSat) > math.MaxInt64 ||
		invoice.Timestamp.After(p.cfg.Now()) ||
		invoice.MinFinalCLTVExpiry() < uint64(p.cfg.MinFinalCLTV) ||
		invoice.MinFinalCLTVExpiry() > math.MaxUint16 ||
		!invoice.Timestamp.Add(invoice.Expiry()).After(
			p.cfg.Now().Add(p.cfg.PaymentTimeout+30*time.Second),
		) {

		return nil, errors.New("unsafe payment invoice")
	}

	return invoice, nil
}

// Probe uses only LND's estimator. LND constructs a different random payment
// hash, so knowing the supplied invoice's preimage cannot settle the probe.
// A successful result indicates reachability, not reserved liquidity.
func (p *LndPayments) Probe(ctx context.Context,
	encoded string) (ProbeOutcome, uint64, error) {

	if _, err := p.decode(encoded); err != nil {
		return ProbeFailed, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ProbeTimeout)
	defer cancel()
	select {
	case p.probeSlot <- struct{}{}:
		defer func() { <-p.probeSlot }()

	case <-ctx.Done():
		return ProbeTimedOut, 0, ctx.Err()
	}
	result, err := p.cfg.Router.EstimateRouteFee(ctx,
		&routerrpc.RouteFeeRequest{
			PaymentRequest: encoded,
			Timeout:        uint32(p.cfg.ProbeTimeout / time.Second),
		},
	)
	if status.Code(err) == codes.Unimplemented {
		return ProbeUnsupported, 0, nil
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		status.Code(err) == codes.DeadlineExceeded {

		return ProbeTimedOut, 0, nil
	}
	if err != nil {
		return ProbeFailed, 0, err
	}
	if result == nil || result.RoutingFeeMsat < 0 {
		return ProbeFailed, 0, errors.New("invalid route probe result")
	}
	switch result.FailureReason {
	case lnrpc.PaymentFailureReason_FAILURE_REASON_NONE:
		return ProbeSucceeded, uint64(result.RoutingFeeMsat), nil

	case lnrpc.PaymentFailureReason_FAILURE_REASON_TIMEOUT:
		return ProbeTimedOut, 0, nil

	default:
		return ProbeFailed, 0, nil
	}
}

func (p *LndPayments) node(ctx context.Context, want *btcec.PublicKey,
	synced bool) (*btcec.PublicKey, error) {

	info, err := p.cfg.Node.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return nil, err
	}
	if info == nil || (synced && (!info.SyncedToChain ||
		info.BlockHeight == 0 || info.BlockHeight > math.MaxInt32)) {

		return nil, errors.New("paying node is not synchronized")
	}
	raw, err := hex.DecodeString(info.IdentityPubkey)
	if err != nil || len(raw) != 33 ||
		hex.EncodeToString(raw) != info.IdentityPubkey {

		return nil, errors.New("invalid paying node key")
	}
	key, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, err
	}
	if want != nil && !want.IsEqual(key) {
		return nil, errors.New("paying node differs from saved identity")
	}
	return key, nil
}

// Prepare checks the approved quote and available BTC before returning one
// exact LND request. One part avoids asset rounding losses from splitting.
func (p *LndPayments) Prepare(ctx context.Context,
	r *Reservation) (*PaymentPlan, error) {

	if err := r.checkPrepay(); err != nil {
		return nil, err
	}
	if err := p.ValidateQuote(ctx, r.Quote); err != nil {
		return nil, err
	}
	invoice, err := p.decode(r.Quote.PrepayInvoice)
	if err != nil {
		return nil, err
	}
	key, err := p.node(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: r.Quote.PrepayInvoice,
		FeeLimitMsat:   int64(r.MaxRouteFeeMsat),
		TimeoutSeconds: int32(p.cfg.PaymentTimeout / time.Second),
		CltvLimit:      int32(p.cfg.CLTVLimit),
		MaxParts:       1,
	}
	if err := p.checkRequest(ctx, req); err != nil {
		return nil, err
	}
	return &PaymentPlan{
		Hash:    *invoice.PaymentHash,
		NodeKey: key,
		Request: req,
	}, nil
}

func (p *LndPayments) checkRequest(ctx context.Context,
	req *routerrpc.SendPaymentRequest) error {

	if req == nil {
		return errors.New("missing prepay payment request")
	}
	// These are the only fields this adapter sets. Refuse requests that
	// could redirect the payment, change its amount, or debit assets.
	expected := &routerrpc.SendPaymentRequest{
		PaymentRequest: req.PaymentRequest,
		FeeLimitMsat:   req.FeeLimitMsat,
		TimeoutSeconds: req.TimeoutSeconds,
		CltvLimit:      req.CltvLimit,
		MaxParts:       1,
	}
	if !proto.Equal(req, expected) || req.FeeLimitMsat < 0 ||
		req.TimeoutSeconds <= 0 ||
		req.TimeoutSeconds > int32(p.cfg.PaymentTimeout/time.Second) ||
		req.CltvLimit <= 0 || uint32(req.CltvLimit) > p.cfg.CLTVLimit {

		return errors.New("invalid saved prepay request")
	}
	invoice, err := p.decode(req.PaymentRequest)
	if err != nil {
		return err
	}
	needed := invoice.MinFinalCLTVExpiry() + uint64(routing.BlockPadding)
	if len(invoice.RouteHints) != 1 || len(invoice.RouteHints[0]) != 1 {
		return errors.New("prepay must use one edge")
	}
	needed += uint64(invoice.RouteHints[0][0].CLTVExpiryDelta)
	if needed > uint64(req.CltvLimit) ||
		uint64(req.FeeLimitMsat) > math.MaxInt64-uint64(*invoice.MilliSat) {

		return errors.New("prepay exceeds route or amount limit")
	}
	required := uint64(*invoice.MilliSat) + uint64(req.FeeLimitMsat)
	channels, err := p.cfg.Node.ListChannels(ctx,
		&lnrpc.ListChannelsRequest{
			ActiveOnly: true,
		},
	)
	if err != nil {
		return err
	}
	for _, ch := range channels.GetChannels() {
		if ch == nil || !ch.Active || len(ch.CustomChannelData) != 0 ||
			ch.LocalConstraints == nil || ch.LocalBalance <= 0 ||
			uint64(ch.LocalBalance) <= ch.LocalConstraints.ChanReserveSat {

			continue
		}
		sats := uint64(ch.LocalBalance) - ch.LocalConstraints.ChanReserveSat
		if sats <= math.MaxInt64/1000 && sats*1000 >= required {
			return nil
		}
	}
	return errors.New("insufficient single-channel BTC balance for prepay")
}

// Lookup reads the saved node and hash even after invoice expiry. Only an
// authoritative NotFound permits the FSM to consider a first send.
func (p *LndPayments) Lookup(ctx context.Context, node *btcec.PublicKey,
	hash lntypes.Hash) (*lnrpc.Payment, error) {

	if node == nil || hash == (lntypes.Hash{}) {
		return nil, errors.New("missing saved payment identity")
	}
	if _, err := p.node(ctx, node, false); err != nil {
		return nil, err
	}
	stream, err := p.cfg.Router.TrackPaymentV2(ctx,
		&routerrpc.TrackPaymentRequest{
			PaymentHash: hash[:],
		},
	)
	if err == nil {
		var result *lnrpc.Payment
		result, err = stream.Recv()
		if err == nil {
			if result == nil || result.PaymentHash != hash.String() {
				return nil, errors.New("payment lookup hash mismatch")
			}
			return result, nil
		}
	}
	if status.Code(err) == codes.NotFound {
		return nil, ErrPaymentNotFound
	}
	return nil, err
}

// Pay sends the persisted request on its saved node. Any error leaves the
// outcome uncertain; Lookup, not a new hash, resolves it.
func (p *LndPayments) Pay(ctx context.Context, node *btcec.PublicKey,
	req *routerrpc.SendPaymentRequest) error {

	if node == nil {
		return errors.New("missing saved paying node")
	}
	if _, err := p.node(ctx, node, true); err != nil {
		return err
	}
	if err := p.checkRequest(ctx, req); err != nil {
		return err
	}
	stream, err := p.cfg.Router.SendPaymentV2(ctx,
		proto.Clone(req).(*routerrpc.SendPaymentRequest),
	)
	if err != nil {
		return err
	}
	_, err = stream.Recv()
	return err
}

var _ Payments = (*LndPayments)(nil)
