package assetexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	depositflow "github.com/lightninglabs/loop/staticaddr/deposit/flow"
	"github.com/lightninglabs/loop/staticaddr/loopin/flow"
)

// Controller serializes effects by swap and commits every exposure boundary
// before performing its external effect. Run one controller per local store.
type Controller struct {
	store   Store
	actions Actions
	now     func() time.Time
	locks   [64]chan struct{}
}

// NewController connects a durable journal and authenticated local adapters.
func NewController(store Store, actions Actions,
	now func() time.Time) *Controller {

	if now == nil {
		now = time.Now
	}
	c := &Controller{store: store, actions: actions, now: now}
	for i := range c.locks {
		c.locks[i] = make(chan struct{}, 1)
	}
	return c
}

func (c *Controller) lock(ctx context.Context, hash [32]byte) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock := c.locks[hash[0]%64]
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case lock <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-lock
			return nil, err
		}
		return func() { <-lock }, nil
	}
}

// Start journals the immutable offer and invoice intent before reserving the
// deposits. Retrying the identical request resumes an interrupted reservation.
func (c *Controller) Start(ctx context.Context, offer Offer,
	intent []byte) error {

	unlock, err := c.lock(ctx, offer.Hash)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.store.Get(ctx, offer.Hash)
	if err == nil {
		if !same(r.Offer, offer) ||
			!bytes.Equal(r.InvoiceIntent, intent) {

			return ErrConflict
		}
		if r.State.Terminal() || r.CancellationRequested {
			return ErrState
		}
		return c.reserve(ctx, r)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := validateOffer(offer); err != nil {
		return err
	}
	if len(intent) == 0 || !c.now().Before(offer.Terms.OfferExpiry) {
		return ErrInvalid
	}
	if err := c.actions.ValidateOffer(ctx, cloneOffer(offer)); err != nil {
		return err
	}
	r = &Record{
		State: InitHtlcTx, Offer: cloneOffer(offer),
		DepositState:  depositflow.Deposited,
		InvoiceIntent: bytes.Clone(intent),
	}
	if err := c.store.Create(ctx, r); err != nil {
		return err
	}
	return c.reserve(ctx, r)
}

func (c *Controller) reserve(ctx context.Context, r *Record) error {
	if r.Reserved {
		return nil
	}
	if err := c.actions.ReserveDeposits(
		ctx, cloneOffer(r.Offer),
	); err != nil {
		return err
	}
	if err := transitionDeposit(
		r, depositflow.OnLoopInInitiated,
	); err != nil {
		return err
	}
	r.Reserved = true
	return c.save(ctx, r)
}

// Status returns a private independent snapshot for local recovery only.
func (c *Controller) Status(ctx context.Context, hash [32]byte) (
	*Record, error) {

	return c.store.Get(ctx, hash)
}

// PrepareInvoice persists creation intent before the RPC and its exact result
// before returning it. An ambiguous creation is looked up by the same hash;
// the adapter must recover the original RFQ binding, not negotiate a new one.
func (c *Controller) PrepareInvoice(ctx context.Context, hash [32]byte) (
	Invoice, error) {

	unlock, err := c.lock(ctx, hash)
	if err != nil {
		return Invoice{}, err
	}
	defer unlock()
	r, err := c.store.Get(ctx, hash)
	if err != nil {
		return Invoice{}, err
	}
	if r.CancellationRequested || r.State.Terminal() {
		return Invoice{}, ErrState
	}
	if r.Invoice.PaymentRequest != "" {
		return cloneInvoice(r.Invoice), nil
	}
	if r.State != InitHtlcTx {
		return Invoice{}, ErrState
	}
	if err := c.reserve(ctx, r); err != nil {
		return Invoice{}, err
	}
	var invoice Invoice
	if r.InvoiceCreating {
		invoice, err = c.actions.LookupInvoice(ctx, cloneOffer(r.Offer))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return Invoice{}, err
		}
	}
	if invoice.PaymentRequest == "" {
		if !c.now().Before(r.Offer.Terms.OfferExpiry) {
			return Invoice{}, ErrState
		}
		if !r.InvoiceCreating {
			r.InvoiceCreating = true
			if err := c.save(ctx, r); err != nil {
				return Invoice{}, err
			}
		}
		invoice, err = c.actions.CreateInvoice(
			ctx, cloneOffer(r.Offer), bytes.Clone(r.InvoiceIntent),
		)
		if err != nil {
			return Invoice{}, err
		}
	}
	if invoice.PaymentRequest == "" || len(invoice.PaymentPlan) == 0 {
		return Invoice{}, ErrInvalid
	}
	r.Invoice = cloneInvoice(invoice)
	if err := transition(r, flow.OnHtlcInitiated); err != nil {
		return Invoice{}, err
	}
	if err := c.save(ctx, r); err != nil {
		return Invoice{}, err
	}
	return cloneInvoice(invoice), nil
}

// ReceiveFallback verifies and finalizes the SDK recovery bundle, then stores
// it before any client signature is permitted to leave this controller.
func (c *Controller) ReceiveFallback(ctx context.Context, hash [32]byte,
	bundle []byte) error {

	unlock, err := c.lock(ctx, hash)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.store.Get(ctx, hash)
	if err != nil {
		return err
	}
	if len(r.ServerFallback) != 0 {
		if !bytes.Equal(r.ServerFallback, bundle) {
			return ErrConflict
		}
		return nil
	}
	if r.State != SignHtlcTx || r.CancellationRequested ||
		len(bundle) == 0 {

		return ErrState
	}
	fallback, err := c.actions.VerifyFinalizeFallback(
		ctx, cloneOffer(r.Offer), bytes.Clone(bundle),
	)
	if err != nil {
		return err
	}
	if len(fallback.Bundle) == 0 || len(fallback.ClaimSignatures) == 0 {
		return ErrInvalid
	}
	r.ServerFallback = bytes.Clone(bundle)
	r.Fallback = cloneFallback(fallback)
	return c.save(ctx, r)
}

// ExposeClaim independently rechecks the local accepted asset HTLC, then
// durably records exposure before sending signatures. An uncertain send keeps
// the signatures exposed; retries send exactly the persisted signature bytes.
func (c *Controller) ExposeClaim(ctx context.Context, hash [32]byte) error {
	unlock, err := c.lock(ctx, hash)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.store.Get(ctx, hash)
	if err != nil {
		return err
	}
	if r.CancellationRequested || r.State.Terminal() ||
		r.HasPreimage || len(r.Fallback.Bundle) == 0 {

		return ErrState
	}
	// A durable exposure marker means signatures may have escaped, but
	// a crash may also have happened before the first send. Never reveal
	// claim authority for the first time after the payout has disappeared.
	if !c.now().Before(r.Offer.Terms.OfferExpiry) {
		return ErrState
	}
	evidence, err := c.actions.Prepared(ctx, cloneOffer(r.Offer))
	if err != nil {
		return err
	}
	if len(evidence) == 0 {
		return ErrPending
	}
	if r.SignaturesExposed && !bytes.Equal(evidence, r.AcceptedEvidence) {
		return ErrConflict
	}
	if err := c.checkExposure(ctx, r); err != nil {
		return err
	}
	if !r.SignaturesExposed {
		if r.State != SignHtlcTx {
			return ErrState
		}
		r.AcceptedEvidence = bytes.Clone(evidence)
		r.SignaturesExposed = true
		if err := transition(r, flow.OnHtlcTxSigned); err != nil {
			return err
		}
		if err := c.save(ctx, r); err != nil {
			return err
		}
	}
	// The journal flush can cross a deadline too. Keep the exposure marker
	// if uncertain, but never send after the safe claim window is gone.
	if err := c.checkExposure(ctx, r); err != nil {
		return err
	}
	return c.actions.PushSignatures(
		ctx,
		cloneOffer(r.Offer),
		bytes.Clone(r.Fallback.ClaimSignatures),
	)
}

// checkExposure rechecks the safe claim window after blocking operations.
func (c *Controller) checkExposure(ctx context.Context, r *Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	height, err := c.actions.CurrentHeight(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t := r.Offer.Terms
	if !c.now().Before(t.OfferExpiry) ||
		uint64(
			height,
		)+uint64(
			t.ExecutionDelta,
		) >= uint64(
			t.RefundHeight,
		) {

		return ErrState
	}
	return nil
}

// ReceivePreimage verifies and journals the server's committed preimage before
// settling the local HOLD invoice. Settlement completion is checked locally.
func (c *Controller) ReceivePreimage(ctx context.Context, hash,
	preimage [32]byte) error {

	unlock, err := c.lock(ctx, hash)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.store.Get(ctx, hash)
	if err != nil {
		return err
	}
	if sha256.Sum256(preimage[:]) != hash || !r.SignaturesExposed ||
		r.State == Failed || r.State == HtlcTimeoutSwept {

		return ErrInvalid
	}
	if !r.HasPreimage {
		r.Preimage = preimage
		r.HasPreimage = true
		if err := c.save(ctx, r); err != nil {
			return err
		}
	}
	return c.settle(ctx, r)
}

func (c *Controller) settle(ctx context.Context, r *Record) error {
	if r.State == PaymentReceived || r.State == Succeeded {
		return nil
	}
	if err := c.actions.SettleInvoice(
		ctx, cloneOffer(r.Offer), r.Preimage,
	); err != nil {
		return err
	}
	settled, err := c.actions.InvoiceSettled(ctx, cloneOffer(r.Offer))
	if err != nil {
		return err
	}
	if !settled {
		return ErrPending
	}
	if err := transition(r, flow.OnPaymentReceived); err != nil {
		return err
	}
	return c.save(ctx, r)
}

// CooperativeSignature signs only after local settlement, with an immutable
// request committed before signing and the resulting signature before return.
func (c *Controller) CooperativeSignature(ctx context.Context, hash [32]byte,
	request []byte) ([]byte, error) {

	unlock, err := c.lock(ctx, hash)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := c.store.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	if (r.State != PaymentReceived && r.State != Succeeded) ||
		len(request) == 0 {

		return nil, ErrState
	}
	if len(r.CooperativeRequest) != 0 &&
		!bytes.Equal(r.CooperativeRequest, request) {

		return nil, ErrConflict
	}
	settled, err := c.actions.InvoiceSettled(ctx, cloneOffer(r.Offer))
	if err != nil {
		return nil, err
	}
	if !settled {
		return nil, ErrPending
	}
	if len(r.CooperativeSignature) != 0 {
		return bytes.Clone(r.CooperativeSignature), nil
	}
	if r.State == Succeeded {
		return nil, ErrState
	}
	if len(r.CooperativeRequest) == 0 {
		r.CooperativeRequest = bytes.Clone(request)
		if err := c.save(ctx, r); err != nil {
			return nil, err
		}
	}
	signature, err := c.actions.SignCooperative(
		ctx, cloneOffer(r.Offer), bytes.Clone(request),
	)
	if err != nil {
		return nil, err
	}
	if len(signature) == 0 {
		return nil, ErrInvalid
	}
	r.CooperativeSignature = bytes.Clone(signature)
	if err := c.save(ctx, r); err != nil {
		return nil, err
	}
	return bytes.Clone(signature), nil
}

// Cancel persists cancellation intent. Once signatures might have escaped,
// only confirmed refunds with spendable asset proofs allow invoice cancellation
// or deposit release. Merely reaching the refund height never does so.
func (c *Controller) Cancel(ctx context.Context, hash [32]byte) error {
	unlock, err := c.lock(ctx, hash)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.store.Get(ctx, hash)
	if err != nil {
		return err
	}
	if r.State.Terminal() {
		return c.release(ctx, r)
	}
	if r.State == PaymentReceived {
		return ErrState
	}
	// Learning the secret does not prove payment. In particular, the
	// incoming HTLC may already have failed when a late claim reveals it.
	// Preserve the secret, but allow independent confirmed-refund recovery
	// if the local invoice never settled. Also check when the secret is
	// absent: local settlement can precede a process crash.
	if len(r.AcceptedEvidence) != 0 {
		settled, err := c.actions.InvoiceSettled(
			ctx,
			cloneOffer(r.Offer),
		)
		if err != nil {
			return err
		}
		if settled {
			if !r.HasPreimage {
				return ErrPending
			}
			if err := transition(
				r, flow.OnPaymentReceived,
			); err != nil {
				return err
			}
			if err := c.save(ctx, r); err != nil {
				return err
			}
			return ErrState
		}
	}
	if !r.CancellationRequested {
		r.CancellationRequested = true
		if r.SignaturesExposed {
			if err := transition(
				r, flow.OnSweepHtlcTimeout,
			); err != nil {
				return err
			}
		}
		if err := c.save(ctx, r); err != nil {
			return err
		}
	}
	if r.SignaturesExposed {
		// A previously persisted refund proof can become stale across
		// a reorg and restart. Recheck current confirmation and
		// spendable asset proofs before irreversible cancellation.
		evidence, err := c.actions.RecoverRefund(
			ctx, cloneOffer(r.Offer), cloneFallback(r.Fallback),
		)
		if err != nil {
			return err
		}
		if len(evidence) == 0 {
			return ErrPending
		}
		if !bytes.Equal(r.RefundEvidence, evidence) {
			r.RefundEvidence = bytes.Clone(evidence)
			if err := c.save(ctx, r); err != nil {
				return err
			}
		}
	}
	if err := c.actions.CancelInvoice(
		ctx, cloneOffer(r.Offer),
	); err != nil {
		return err
	}
	event := flow.OnCanceled
	if r.SignaturesExposed {
		event = flow.OnHtlcTimeoutSwept
	}
	if err := transition(r, event); err != nil {
		return err
	}
	if err := c.save(ctx, r); err != nil {
		return err
	}
	return c.release(ctx, r)
}

// Recover resumes durable work after restart or an ambiguous RPC response.
// It never replaces a quote, fallback, signature, or payment hash.
func (c *Controller) Recover(ctx context.Context, hash [32]byte) error {
	r, err := c.store.Get(ctx, hash)
	if err != nil {
		return err
	}
	if r.State.Terminal() {
		unlock, err := c.lock(ctx, hash)
		if err != nil {
			return err
		}
		defer unlock()
		r, err = c.store.Get(ctx, hash)
		if err != nil {
			return err
		}
		return c.release(ctx, r)
	}
	recoveryState, err := flow.Next(
		flow.ServerAsset,
		r.State,
		flow.OnRecover,
	)
	if err != nil {
		return ErrState
	}
	var recoveryErr error
	if r.SignaturesExposed && !r.HasPreimage && r.State != PaymentReceived {
		preimage, found, err := c.lookupPreimage(ctx, r.Offer)
		recoveryErr = err
		if err == nil && found {
			r.Preimage, r.HasPreimage = preimage, true
		}
	}
	if r.HasPreimage && r.State != PaymentReceived {
		recoveryErr = c.ReceivePreimage(ctx, hash, r.Preimage)
		if recoveryErr == nil {
			return nil
		}
	}
	if r.State != PaymentReceived {
		due, err := c.refundDue(ctx, r)
		if err != nil {
			return errors.Join(recoveryErr, err)
		}
		if due {
			// Server availability cannot veto recovery.
			// Cancel checks local settlement, journals intent,
			// then drives the signed refund bundle.
			refundErr := c.Cancel(ctx, hash)
			if refundErr == nil {
				return nil
			}
			return errors.Join(recoveryErr, refundErr)
		}
	}
	if recoveryErr != nil {
		return recoveryErr
	}
	switch recoveryState {
	case InitHtlcTx:
		_, err := c.PrepareInvoice(ctx, hash)
		return err
	case SignHtlcTx:
		if len(r.Fallback.Bundle) == 0 {
			return ErrPending
		}
		return c.ExposeClaim(ctx, hash)
	case MonitorInvoiceAndHtlcTx:
		return c.ExposeClaim(ctx, hash)
	case PaymentReceived:
		return c.confirmClaim(ctx, hash)
	default:
		return ErrPending
	}
}

// lookupPreimage leaves most of the action budget available for local
// settlement checks and independent refund recovery if discovery stalls.
func (c *Controller) lookupPreimage(ctx context.Context, offer Offer) (
	[32]byte, bool, error) {

	timeout := time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline)/4)
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.actions.LookupPreimage(discoveryCtx, cloneOffer(offer))
}

// refundDue treats deadline expiry as a request to recover, never as proof
// that signatures, invoices, or deposits are safe to release.
func (c *Controller) refundDue(ctx context.Context, r *Record) (bool, error) {
	if r.CancellationRequested ||
		!c.now().Before(r.Offer.Terms.OfferExpiry) {

		return true, nil
	}
	if !r.SignaturesExposed {
		return false, nil
	}
	height, err := c.actions.CurrentHeight(ctx)
	return height >= r.Offer.Terms.RefundHeight, err
}

func (c *Controller) confirmClaim(ctx context.Context, hash [32]byte) error {
	unlock, err := c.lock(ctx, hash)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.store.Get(ctx, hash)
	if err != nil {
		return err
	}
	if r.State != PaymentReceived {
		return ErrState
	}
	evidence, err := c.actions.InspectClaim(
		ctx, cloneOffer(r.Offer), cloneFallback(r.Fallback),
	)
	if err != nil {
		return err
	}
	if len(evidence) == 0 {
		return ErrPending
	}
	r.ClaimEvidence = bytes.Clone(evidence)
	if err := transition(r, flow.OnSucceeded); err != nil {
		return err
	}
	if err := c.save(ctx, r); err != nil {
		return err
	}
	return c.release(ctx, r)
}

func (c *Controller) release(ctx context.Context, r *Record) error {
	if r.DepositsReleased {
		return nil
	}
	if err := c.actions.ReleaseDeposits(
		ctx, cloneOffer(r.Offer),
	); err != nil {
		return err
	}
	r.DepositsReleased = true
	return c.save(ctx, r)
}

func (c *Controller) save(ctx context.Context, r *Record) error {
	previous := r.Revision
	r.Revision++
	return c.store.Update(ctx, r, previous)
}

func validateOffer(offer Offer) error {
	t := offer.Terms
	key, err := btcec.ParsePubKey(t.GroupKey[:])
	if err != nil ||
		!bytes.Equal(key.SerializeCompressed(), t.GroupKey[:]) {

		return ErrInvalid
	}
	if offer.Hash == ([32]byte{}) || t.Version != 1 ||
		t.RequestID == ([32]byte{}) ||
		t.RequestDigest == ([32]byte{}) ||
		t.Owner == ([32]byte{}) || t.GroupKey == ([33]byte{}) ||
		len(t.DepositOutpoints) == 0 || len(t.Contract) == 0 ||
		t.PayoutAmount == 0 || t.MaxDebit < t.PayoutAmount ||
		t.RefundHeight == 0 || t.RecoveryDelta == 0 ||
		t.ExecutionDelta == 0 || t.OfferExpiry.IsZero() ||
		t.PayoutAmount > math.MaxUint64-t.AssetFee ||
		t.PayoutAmount+t.AssetFee > math.MaxUint64-t.ChangeAmount ||
		t.DepositAmount != t.PayoutAmount+t.AssetFee+t.ChangeAmount ||
		uint64(t.MinPayoutExpiry) <= uint64(t.RefundHeight)+
			uint64(t.RecoveryDelta)+uint64(t.ExecutionDelta) {

		return ErrInvalid
	}
	seen := make(map[string]bool)
	for _, outpoint := range t.DepositOutpoints {
		parts := strings.Split(outpoint, ":")
		if len(parts) != 2 || seen[outpoint] {
			return ErrInvalid
		}
		txid, err := hex.DecodeString(parts[0])
		if err != nil || len(txid) != 32 ||
			hex.EncodeToString(txid) != parts[0] {

			return ErrInvalid
		}
		index, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil || strconv.FormatUint(index, 10) != parts[1] {
			return ErrInvalid
		}
		seen[outpoint] = true
	}
	return nil
}

func same(a, b any) bool {
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	return err == nil && bytes.Equal(x, y)
}

func cloneOffer(o Offer) Offer {
	o.Terms.Contract = bytes.Clone(o.Terms.Contract)
	o.Terms.DepositOutpoints = append(
		[]string(nil),
		o.Terms.DepositOutpoints...)
	return o
}

func cloneInvoice(i Invoice) Invoice {
	i.PaymentPlan = bytes.Clone(i.PaymentPlan)
	return i
}

func cloneFallback(f Fallback) Fallback {
	f.Bundle = bytes.Clone(f.Bundle)
	f.ClaimSignatures = bytes.Clone(f.ClaimSignatures)
	return f
}
