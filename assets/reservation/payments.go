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
	"github.com/lightninglabs/lndclient"
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
// server's independent edge delivers the quoted asset fee. Authenticated raw
// clients preserve the saved request and full payment response used in recovery.
type PaymentConfig struct {
	// Node supplies authenticated RPCs for checking the paying node's
	// identity, chain sync, and available BTC channel balance.
	Node lndclient.ServiceClient[lnrpc.LightningClient]

	// Router supplies authenticated probe, send, and payment-tracking RPCs.
	// It must connect to the same LND instance as Node.
	Router lndclient.ServiceClient[routerrpc.RouterClient]

	// Params identifies the Bitcoin network accepted when decoding invoices.
	Params *chaincfg.Params

	// Now supplies the clock used to check invoice and quote expiry.
	Now func() time.Time

	// PaymentTimeout limits how long LND may attempt to route the prepay.
	// It must be a positive whole number of seconds, at most one minute.
	// Quotes and invoices must remain valid for this duration plus 30 seconds.
	// This is not a deadline for settling an already dispatched HTLC.
	PaymentTimeout time.Duration

	// ProbeTimeout bounds the sole probe attempt before requesting receiver
	// cancellation. It is also passed to LND. It must be a positive
	// whole number of seconds, at most one minute.
	ProbeTimeout time.Duration

	// CLTVLimit caps the prepay route's total timelock delta, in blocks,
	// including the final hop and routing safety padding.
	CLTVLimit uint32

	// MinFinalCLTV is the minimum final-hop timelock delta, in blocks,
	// accepted in either the prepay invoice or the main-payment probe invoice.
	MinFinalCLTV uint32
}

// LndPayments validates, probes, and pays without owning purchase state.
// The FSM saves the returned request before dispatch and reconciles its hash.
type LndPayments struct {
	cfg PaymentConfig
}

// NewLndPayments checks the required clients, clock, network, and timing and
// CLTV limits, then creates the adapter. It makes no node
// calls and does not change the user's approved quote or routing fee limit.
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
		cfg: cfg,
	}, nil
}

// ValidateQuote checks that the prepay invoice matches the asset fee under the
// server-supplied receiving RFQ. Both invoices must match the quoted BTC amounts,
// receiver, and edge, and remain valid long enough to attempt payment. The main
// invoice supplies an estimate for probing; it does not lock the later swap rate.
//
// These are local consistency and payment-safety checks. The server supplies
// both the RFQ and invoices, so passing does not independently prove that the RFQ
// was negotiated or will be honored. This method neither probes nor pays, and
// does not establish user approval; the FSM binds approval to the quote hash.
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
	var probeRFQ rfqrpc.PeerAcceptedBuyQuote
	if len(q.ReservationId) != 32 || len(q.ProbeRfq) == 0 ||
		len(q.ProbeRfq) > 1<<20 {

		return errors.New("missing asset probe quote")
	}
	if err := proto.Unmarshal(q.ProbeRfq, &probeRFQ); err != nil {
		return err
	}
	_, err = payment.ValidateInvoice(q.ProbeInvoice, &probeRFQ,
		payment.InvoiceTerms{
			AssetID:      terms.AssetID,
			Amount:       terms.Amount,
			Hash:         ProbeHash(ID(q.ReservationId)),
			Payee:        [33]byte(q.ReceivingNodeKey),
			MinFinalCLTV: uint64(p.cfg.MinFinalCLTV),
			MinLifetime:  p.cfg.PaymentTimeout + 30*time.Second,
		}, p.cfg.Params, p.cfg.Now(),
	)
	if err != nil {
		return err
	}
	if probeRFQ.Peer != hex.EncodeToString(q.EdgeKey) {
		return errors.New("probe changed edge")
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

// decode parses a size-bounded BOLT11 invoice for the configured network. It
// requires a nonzero payment hash, recipient, positive amount, acceptable final
// CLTV delta, and enough remaining lifetime for payment plus a safety margin.
// Matching those values to the reservation and RFQ belongs to ValidateQuote.
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

// PrepareProbe constructs the sole, non-settleable payment for this quote.
// The caller saves the returned request and node before dispatch.
func (p *LndPayments) PrepareProbe(ctx context.Context,
	r *Reservation) (*PaymentPlan, error) {

	if err := p.ValidateQuote(ctx, r.Quote); err != nil {
		return nil, err
	}
	node, err := p.node(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	// A probe precedes approval. Use a bounded routing budget independently
	// of the later, user-approved prepay fee cap.
	fee := r.Quote.EstimatedMainAmountMsat/50 + 10_000
	return &PaymentPlan{
		Hash: ProbeHash(r.ID), NodeKey: node,
		Request: &routerrpc.SendPaymentRequest{
			PaymentRequest: r.Quote.ProbeInvoice,
			FeeLimitMsat:   int64(fee),
			TimeoutSeconds: int32(p.cfg.ProbeTimeout / time.Second),
			CltvLimit:      int32(p.cfg.CLTVLimit),
			MaxParts:       1,
		},
	}, nil
}

// SendProbe streams updates from dispatch so in-flight routes can be saved
// before LND prunes failed attempts. The observer must persist updates inline.
func (p *LndPayments) SendProbe(ctx context.Context, r *Reservation,
	observe func(*lnrpc.Payment) error) error {

	if err := p.ValidateQuote(ctx, r.Quote); err != nil {
		return err
	}
	if _, err := p.node(ctx, r.ProbeNodeKey, true); err != nil {
		return err
	}
	req := r.ProbeRequest
	if req == nil {
		return errors.New("missing probe request")
	}
	expected := &routerrpc.SendPaymentRequest{
		PaymentRequest: r.Quote.ProbeInvoice,
		FeeLimitMsat:   int64(r.Quote.EstimatedMainAmountMsat/50 + 10_000),
		TimeoutSeconds: req.TimeoutSeconds,
		CltvLimit:      int32(p.cfg.CLTVLimit), MaxParts: 1,
	}
	if !proto.Equal(req, expected) || req.TimeoutSeconds <= 0 ||
		time.Duration(req.TimeoutSeconds)*time.Second > p.cfg.ProbeTimeout {

		return errors.New("invalid probe request")
	}
	ctx, cancel := context.WithDeadline(ctx, r.ProbeDeadline)
	defer cancel()
	rpcCtx, _, router := p.cfg.Router.RawClientWithMacAuth(ctx)
	// The node lookup may have consumed the remaining dispatch budget.
	// Never send a fresh HTLC after the saved deadline or quote expiry.
	now := p.cfg.Now()
	if !now.Before(r.ProbeDeadline) || now.Unix() >= r.Quote.ExpiresAt {
		return context.DeadlineExceeded
	}
	stream, err := router.SendPaymentV2(
		rpcCtx, proto.Clone(req).(*routerrpc.SendPaymentRequest),
	)
	if err != nil {
		return err
	}
	return receiveProbeUpdates(stream, observe)
}

// TrackProbe resumes live observation of the original payment after restart.
func (p *LndPayments) TrackProbe(ctx context.Context, r *Reservation,
	observe func(*lnrpc.Payment) error) error {

	if _, err := p.node(ctx, r.ProbeNodeKey, false); err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, r.ProbeDeadline)
	defer cancel()
	rpcCtx, _, router := p.cfg.Router.RawClientWithMacAuth(ctx)
	hash := ProbeHash(r.ID)
	stream, err := router.TrackPaymentV2(
		rpcCtx, &routerrpc.TrackPaymentRequest{
			PaymentHash: hash[:], NoInflightUpdates: false,
		},
	)
	if err != nil {
		return err
	}
	return receiveProbeUpdates(stream, observe)
}

// receiveProbeUpdates keeps the stream open through route attempts until the
// payment resolves. A failed observation stops further reads and persistence.
func receiveProbeUpdates(stream interface {
	Recv() (*lnrpc.Payment, error)
}, observe func(*lnrpc.Payment) error) error {
	for {
		update, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := observe(update); err != nil {
			return err
		}
		if update.Status == lnrpc.Payment_FAILED ||
			update.Status == lnrpc.Payment_SUCCEEDED {

			return nil
		}
	}
}

// node reads and validates the connected LND's identity. If want is set, the
// identity must match the saved paying node so recovery cannot mistake another
// node's missing payment for permission to send again. When synced is true, LND
// must also report chain sync and a usable block height before preparing or
// sending payment; looking up an existing payment does not require chain sync.
func (p *LndPayments) node(ctx context.Context, want *btcec.PublicKey,
	synced bool) (*btcec.PublicKey, error) {

	rpcCtx, _, node := p.cfg.Node.RawClientWithMacAuth(ctx)
	info, err := node.GetInfo(rpcCtx, &lnrpc.GetInfoRequest{})
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

// Prepare checks approval, quote validity, node identity and sync, and available
// BTC before returning the payment hash, paying node, and exact LND request.
// The request carries the approved routing cap and permits one part to avoid
// asset conversion rounding across shards. It sends nothing: the FSM must save
// the returned plan before dispatch so recovery can track the same payment.
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

// checkRequest accepts only the fields used by a single-part BTC prepay and
// checks its invoice lifetime, timeout, CLTV budget, and amount arithmetic.
// It also requires one active BTC channel whose balance, after its reserve,
// covers the invoice and full routing cap. This check does not reserve funds
// or establish an end-to-end route, and runs even when probing is skipped.
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
	rpcCtx, _, node := p.cfg.Node.RawClientWithMacAuth(ctx)
	channels, err := node.ListChannels(rpcCtx,
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

// Lookup verifies the saved node identity and returns the first payment update
// for the saved hash, which may still be in flight. It works after invoice
// expiry and checks that the response carries the requested hash. Only LND's
// NotFound becomes ErrPaymentNotFound; other errors leave the outcome unknown
// and must not be treated by the FSM as permission to send again.
func (p *LndPayments) Lookup(ctx context.Context, node *btcec.PublicKey,
	hash lntypes.Hash) (*lnrpc.Payment, error) {

	if node == nil || hash == (lntypes.Hash{}) {
		return nil, errors.New("missing saved payment identity")
	}
	if _, err := p.node(ctx, node, false); err != nil {
		return nil, err
	}
	rpcCtx, _, router := p.cfg.Router.RawClientWithMacAuth(ctx)
	stream, err := router.TrackPaymentV2(rpcCtx,
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

// Pay rechecks the saved node's identity and sync and the request's limits and
// available balance, then sends a copy of the persisted request. It reads only
// the first stream update, so a nil error does not establish settlement. The
// FSM must use Lookup to resolve the saved hash after either success or error;
// this method neither saves a result nor retries the payment.
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
	rpcCtx, _, router := p.cfg.Router.RawClientWithMacAuth(ctx)
	stream, err := router.SendPaymentV2(rpcCtx,
		proto.Clone(req).(*routerrpc.SendPaymentRequest),
	)
	if err != nil {
		return err
	}
	_, err = stream.Recv()
	return err
}

var _ Payments = (*LndPayments)(nil)
