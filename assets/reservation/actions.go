package reservation

import (
	"bytes"
	"context"
	"errors"
	"math"
	"time"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/protobuf/proto"
)

func (f *FSM) selector() *swapserverrpc.AssetReservationSelector {
	return &swapserverrpc.AssetReservationSelector{
		Selector: &swapserverrpc.AssetReservationSelector_ReservationId{
			ReservationId: f.reservation.ID[:],
		},
	}
}

// hasExpiredQuote reports whether a saved quote exists and has expired.
func (f *FSM) hasExpiredQuote() bool {
	return f.reservation.Quote != nil &&
		f.cfg.Clock.Now().Unix() >= f.reservation.Quote.ExpiresAt
}

// RequestQuoteAction repeats the saved request ID after a lost reply.
func (f *FSM) RequestQuoteAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	// Stop after a failed write. Recovery must reload the saved reservation
	// before we make another server request.
	if f.persistErr != nil {
		return fsm.NoOp
	}

	r := f.reservation
	// Resume a saved quote, or cancel if it has expired.
	if r.Quote != nil {
		if f.hasExpiredQuote() {
			return OnCancel
		}

		return OnQuote
	}

	// Reuse the saved ID and key so a lost reply finds the same purchase.
	response, err := f.cfg.Server.QuoteAssetReservation(ctx,
		&swapserverrpc.QuoteAssetReservationRequest{
			ReservationId: r.ID[:],
			AssetId:       r.AssetID[:],
			Amount:        r.Amount,
			ClientKey:     r.ClientKey.PubKey.SerializeCompressed(),
		},
	)
	if err != nil {
		return f.stayInState(err)
	}
	if response == nil || !bytes.Equal(response.ReservationId, r.ID[:]) {
		return f.fail(errors.New("invalid reservation response"))
	}
	if response.State ==
		swapserverrpc.AssetReservationStatus_ASSET_RESERVATION_CANCELED {

		return OnCancel
	}
	if response.Quote == nil {
		// The server has not supplied a quote yet. Stay here and retry.
		return fsm.NoOp
	}
	// Bind the terms to our request, then check the invoices and RFQ
	// before saving anything the user may later approve.
	if err := r.ValidateQuote(response.Quote); err != nil {
		return f.fail(err)
	}
	if err := f.cfg.Payments.ValidateQuote(ctx, response.Quote); err != nil {
		return f.stayInState(err)
	}
	r.Quote = proto.Clone(response.Quote).(*swapserverrpc.AssetReservationQuote)
	r.Terms, _ = TermsFromRPC(r.Quote.Terms)
	// Keep the exact quote across restarts before probing its routes.
	if !f.save(ctx) {
		return fsm.NoOp
	}

	// The quote may have expired while we validated or saved it.
	// CancelPrepay resolves any payment before marking the purchase canceled.
	if f.hasExpiredQuote() {
		return OnCancel
	}

	return OnQuote
}

// ProbeRoutesAction probes the estimated main payment without paying it.
// Normal approval requires success; the prepay fee comes from the same probe.
func (f *FSM) ProbeRoutesAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	if f.persistErr != nil {
		return fsm.NoOp
	}
	// A missing or expired quote cannot proceed to approval. Start prepay
	// cancellation instead of probing routes for an unusable quote.
	if f.reservation.Quote == nil || f.hasExpiredQuote() {
		return OnCancel
	}
	r := f.reservation
	if err := f.cfg.Payments.ValidateQuote(ctx, r.Quote); err != nil {
		return f.stayInState(err)
	}
	// Skipping the probe leaves its result and fee estimates unavailable.
	// AwaitApproval still requires explicit consent to buy without success.
	if r.SkipProbe {
		return OnProbed
	}

	main, mainFee, err := f.cfg.Payments.Probe(ctx, r.Quote.ProbeInvoice)
	// A stopped worker must not replace saved results with a probe that
	// was interrupted by shutdown.
	if ctx.Err() != nil {
		return f.stayInState(ctx.Err())
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		main = ProbeTimedOut

	case err != nil || main == ProbeNotRun || main > ProbeUnsupported ||
		mainFee > math.MaxInt64:

		main = ProbeFailed
	}
	var prepayFee uint64
	if main == ProbeSucceeded {
		prepayFee, err = estimatePrepayRoutingFee(mainFee,
			r.Quote.PrepayAmountMsat, r.Quote.EstimatedMainAmountMsat)
		if err != nil {
			return f.stayInState(err)
		}
	} else {
		// A failed probe provides no fee estimate. The outcome lets
		// callers distinguish unavailable estimates from zero fees.
		mainFee = 0
	}

	// Save the observed outcome and fee estimates before approval. The
	// prepay itself has not been probed, and its fee is only approximate.
	r.Probes = ProbeResults{
		Main:          main,
		PrepayFeeMsat: prepayFee,
		MainFeeMsat:   mainFee,
		CheckedAt:     f.cfg.Clock.Now().UTC().Truncate(time.Microsecond),
	}
	if !f.save(ctx) {
		return fsm.NoOp
	}

	return OnProbed
}

// AwaitApprovalAction requires explicit approval of the exact saved quote.
// Recovery waits here until consent and the PayPrepay transition are saved.
func (f *FSM) AwaitApprovalAction(_ context.Context,
	data fsm.EventContext) fsm.EventType {

	if f.persistErr != nil {
		return fsm.NoOp
	}
	// Approval cannot revive a missing or expired quote. Start prepay
	// cancellation instead of accepting consent on stale terms.
	if f.reservation.Quote == nil || f.hasExpiredQuote() {
		return OnCancel
	}

	r := f.reservation
	// Recovery alone is not consent to pay. Wait for explicit approval.
	if f.event != OnApprove {
		return fsm.NoOp
	}

	a, ok := data.(*looprpc.ApproveAssetReservationRequest)
	if !ok {
		return f.stayInState(errors.New("missing quote approval"))
	}
	if err := r.CheckApproval(a); err != nil {
		return f.stayInState(err)
	}
	// The entry hook saves only the routing cap and SkipProbe choice, in
	// the same transaction as PayPrepay. A failed write leaves us unapproved.
	return OnApproved
}

// observePayment records only a terminal result from the saved paying node.
func (f *FSM) observePayment(ctx context.Context) (bool, error) {
	r := f.reservation
	if r.PaymentResult != nil &&
		(r.PaymentResult.Status == lnrpc.Payment_SUCCEEDED ||
			r.PaymentResult.Status == lnrpc.Payment_FAILED) {

		return true, nil
	}
	result, err := f.cfg.Payments.Lookup(ctx, r.PayingNodeKey, r.PaymentHash)
	if err != nil {
		return false, err
	}
	if result == nil || result.PaymentHash != r.PaymentHash.String() {
		return false, errors.New("invalid prepay payment observation")
	}
	if result.Status != lnrpc.Payment_SUCCEEDED &&
		result.Status != lnrpc.Payment_FAILED {

		// An in-flight payment is unresolved; it must not trigger a resend.
		return false, nil
	}
	next := *r
	next.PaymentResult = proto.Clone(result).(*lnrpc.Payment)
	if err := next.Validate(); err != nil {
		return false, err
	}
	r.PaymentResult = next.PaymentResult
	if !f.save(ctx) {
		return false, f.persistErr
	}
	return true, nil
}

// PayPrepayAction looks up the saved hash before any send. A lost send reply
// or an in-flight payment is not permission for another payment attempt.
func (f *FSM) PayPrepayAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	if f.persistErr != nil {
		return fsm.NoOp
	}
	r := f.reservation
	if r.PaymentRequest == nil {
		// With no saved payment plan, a missing or expired quote must
		// go straight to prepay cancellation.
		if f.reservation.Quote == nil || f.hasExpiredQuote() {
			return OnCancel
		}
		if err := r.checkPrepay(); err != nil {
			return f.fail(err)
		}

		plan, err := f.cfg.Payments.Prepare(ctx, r)
		if err != nil {
			return f.stayInState(err)
		}
		if plan == nil || plan.Request == nil || plan.NodeKey == nil {
			return f.fail(errors.New("missing prepay payment plan"))
		}
		r.PaymentHash, r.PayingNodeKey = plan.Hash, plan.NodeKey
		r.PaymentRequest = plan.Request
		// Save the exact request and node before sending, so a lost reply
		// can be resolved by looking up this payment after a restart.
		if !f.save(ctx) {
			return fsm.NoOp
		}
	}
	// Resolve an existing attempt before checking quote expiry: a settled
	// prepay still entitles the client to delivery after the quote expires.
	terminal, err := f.observePayment(ctx)
	if terminal {
		if r.PaymentResult.Status == lnrpc.Payment_SUCCEEDED {
			return OnPaid
		}
		return OnCancel
	}
	// Only confirmed absence on the saved paying node permits a send.
	// In-flight payments and lookup errors must wait for another lookup.
	if !errors.Is(err, ErrPaymentNotFound) {
		return f.stayInState(err)
	}
	// No payment was found. Cancel the prepay if its quote is now missing
	// or expired, rather than starting a payment on stale terms.
	if f.reservation.Quote == nil || f.hasExpiredQuote() {
		return OnCancel
	}
	// Validate the invoice again after a potentially long offline period.
	if err := f.cfg.Payments.ValidateQuote(ctx, r.Quote); err != nil {
		return f.stayInState(err)
	}
	// Validation can take time; check expiry once more before sending.
	if f.reservation.Quote == nil || f.hasExpiredQuote() {
		return OnCancel
	}

	payErr := f.cfg.Payments.Pay(ctx, r.PayingNodeKey, r.PaymentRequest)

	// A send reply does not establish settlement. Stay here and look up
	// the payment on the next attempt, whether Pay returns an error or nil.
	return f.stayInState(payErr)
}

// WaitForDeliveryAction retrieves delivery facts but never trusts server Ready
// as proof that the client can use the reservation.
func (f *FSM) WaitForDeliveryAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	if f.persistErr != nil {
		return fsm.NoOp
	}
	r := f.reservation
	if r.PaymentResult == nil ||
		r.PaymentResult.Status != lnrpc.Payment_SUCCEEDED {

		return f.fail(errors.New("reservation has no settled prepay"))
	}
	status, err := f.cfg.Server.GetAssetReservation(ctx, f.selector())
	if err != nil {
		return f.stayInState(err)
	}
	if status == nil || !bytes.Equal(status.ReservationId, r.ID[:]) ||
		!proto.Equal(status.Quote, r.Quote) {

		return f.fail(errors.New("server changed the reservation"))
	}
	if status.State ==
		swapserverrpc.AssetReservationStatus_ASSET_RESERVATION_CANCELED {

		// A settled prepay is still owed delivery. Server cancellation
		// is an inconsistency that needs attention, not a normal exit.
		return f.fail(errors.New("server canceled a paid reservation"))
	}
	// Wait until the server can identify the output and supply its proof.
	// Its state alone cannot establish that delivery is valid.
	if status.Outpoint == "" || !status.ProofAvailable {
		return fsm.NoOp
	}
	// The entire service fee was prepaid and must be credited to this
	// reservation for its later swap.
	if status.PrepayCredit != r.Fee {
		return f.fail(errors.New("incorrect settled reservation credit"))
	}
	outpoint, err := ParseOutpoint(status.Outpoint)
	if err != nil {
		return f.fail(err)
	}
	r.FundingOutpoint, r.PrepayCredit = outpoint, status.PrepayCredit
	if !f.save(ctx) {
		return fsm.NoOp
	}
	return OnDelivery
}

// VerifyReservationAction independently checks the full exported proof. An
// offline client uses the original CSV clock, not a new delivery window.
func (f *FSM) VerifyReservationAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	if f.persistErr != nil {
		return fsm.NoOp
	}
	r := f.reservation
	if r.FundingOutpoint == nil {
		return f.fail(errors.New("missing reservation funding outpoint"))
	}
	response, err := f.cfg.Server.GetAssetReservationProof(ctx, f.selector())
	if err != nil {
		return f.stayInState(err)
	}
	if response == nil || !bytes.Equal(response.ReservationId, r.ID[:]) ||
		response.Outpoint != r.FundingOutpoint.String() || len(response.Proof) == 0 {

		return f.fail(errors.New("proof does not match reservation"))
	}
	status, err := f.cfg.Wallet.Verify(ctx, r, response.Proof)
	// A proof mismatch needs attention; node or transport errors can be
	// retried without accepting the delivered output.
	if errors.Is(err, ErrInvalidReservation) {
		return f.fail(err)
	}
	if err != nil {
		return f.stayInState(err)
	}
	if err := f.checkChain(status); err != nil {
		return f.stayInState(err)
	}
	r.ConfirmationHeight = status.ConfirmationHeight
	r.ReservationProof = bytes.Clone(response.Proof)
	// Preserve the verified proof and original confirmation height before
	// declaring readiness or expiry. Recovery must use the same CSV clock.
	if !f.save(ctx) {
		return fsm.NoOp
	}
	lifetime, _ := r.Terms.Lifetime(r.ConfirmationHeight)
	// An offline client may first verify delivery after its CSV lifetime
	// has ended. Such a reservation goes directly to Expired.
	if status.Height >= lifetime.TimeoutHeight {
		return OnTimeout
	}
	if status.Spent {
		return f.fail(errors.New("reservation spent before timeout"))
	}
	return OnProof
}

// checkChain validates local funding heights when verifying delivery and
// monitoring a ready reservation. It requires the quoted confirmation depth,
// preserves any saved first-confirmation height, and checks that the derived
// deadlines fit LND's block-height range. Keeping the confirmation height fixed
// prevents recovery or a chain change from shifting the reservation's expiry.
// The caller then checks whether the reservation has expired or been spent.
func (f *FSM) checkChain(status FundingStatus) error {
	r := f.reservation
	// Require a valid local height and the quoted confirmation depth before
	// treating the output as ready.
	if status.Height > math.MaxInt32 || status.ConfirmationHeight == 0 ||
		status.Height < status.ConfirmationHeight ||
		(status.Height-status.ConfirmationHeight+1) < r.RequiredConfirmations {

		return errors.New("reservation needs confirmed chain data")
	}
	// Do not move an established confirmation height and thereby shift
	// the reservation's expiry during recovery or a chain change.
	if r.ConfirmationHeight != 0 &&
		r.ConfirmationHeight != status.ConfirmationHeight {

		return errors.New("reservation confirmation height changed")
	}
	_, err := r.Terms.Lifetime(status.ConfirmationHeight)
	return err
}

// ReadyAction watches the verified output through the original CSV expiry.
// A later swap must separately enforce its execution cutoff and claim margin.
func (f *FSM) ReadyAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	if f.persistErr != nil {
		return fsm.NoOp
	}
	status, err := f.cfg.Wallet.Inspect(ctx, f.reservation)
	if err != nil {
		return f.stayInState(err)
	}
	if err := f.checkChain(status); err != nil {
		return f.stayInState(err)
	}
	lifetime, _ := f.reservation.Terms.Lifetime(
		f.reservation.ConfirmationHeight,
	)
	// At CSV expiry, finish watching. Before then, an observed spend is
	// unexpected in this purchase flow and needs attention.
	if status.Height >= lifetime.TimeoutHeight {
		return OnTimeout
	}
	if status.Spent {
		return f.fail(errors.New("reservation spent before timeout"))
	}
	return fsm.NoOp
}

// CancelPrepayAction waits for both server cancellation and a resolved local
// payment. A settled payment resumes delivery, even after a cancel request.
func (f *FSM) CancelPrepayAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	if !f.save(ctx) {
		return fsm.NoOp
	}
	resolved := f.reservation.PaymentRequest == nil
	if !resolved {
		terminal, err := f.observePayment(ctx)
		// Settlement wins over cancellation: continue toward delivery
		// even if cancellation was requested or the quote has expired.
		if terminal && f.reservation.PaymentResult.Status == lnrpc.Payment_SUCCEEDED {
			return OnPaid
		}
		if err != nil && !errors.Is(err, ErrPaymentNotFound) {
			return f.stayInState(err)
		}
		resolved = terminal || errors.Is(err, ErrPaymentNotFound)
	}
	// Ask the server to cancel even while a payment is in flight, so its
	// hold invoice can be canceled and the local payment can resolve.
	status, err := f.cfg.Server.CancelAssetReservation(ctx, f.selector())
	if err != nil {
		return f.stayInState(err)
	}
	if status == nil || !bytes.Equal(status.ReservationId, f.reservation.ID[:]) {
		return f.fail(errors.New("invalid reservation cancellation"))
	}
	// Finish only when the local payment is resolved and the server has
	// confirmed cancellation. Either side alone leaves an open obligation.
	if resolved && status.State ==
		swapserverrpc.AssetReservationStatus_ASSET_RESERVATION_CANCELED {

		return OnCanceled
	}
	return fsm.NoOp
}

// NeedAdminAttentionAction retains the purchase and reports the inconsistency.
func (f *FSM) NeedAdminAttentionAction(_ context.Context,
	_ fsm.EventContext) fsm.EventType {

	// Notify only after the state was saved, once per live FSM. Keep the
	// reservation available for investigation and recovery.
	if f.persistErr == nil && !f.adminNotified {
		f.cfg.NotifyAdmin(f.reservation.ID, f.LastActionError)
		f.adminNotified = true
	}
	return fsm.NoOp
}
