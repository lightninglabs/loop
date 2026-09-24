package reservation

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type failingStore struct {
	Store

	fail func(*Reservation) bool
}

func (s *failingStore) UpdateReservation(ctx context.Context,
	r *Reservation) error {

	if s.fail != nil && s.fail(r) {
		return errors.New("injected write failure")
	}
	return s.Store.UpdateReservation(ctx, r)
}

type clientHarness struct {
	swapserverrpc.AssetReservationServiceClient

	t                  *testing.T
	store              *failingStore
	cfg                *Config
	machine            *FSM
	id                 ID
	quote              *swapserverrpc.AssetReservationQuote
	preimage           lntypes.Preimage
	result             *lnrpc.Payment
	probeInvoices      []string
	probeResult        ProbeOutcome
	probePayment       *lnrpc.Payment
	probeCanceled      bool
	probeSucceeded     bool
	sends              int
	quotes             int
	cancels            int
	lookups            int
	notifications      int
	deriveCalls        int
	verifyCalls        int
	quoteErr           error
	quoteValidationErr error
	quotePending       bool
	blockQuote         bool
	quoteStarted       chan struct{}
	fundingUnavailable bool
	deliver            bool
	badProof           bool
	status             FundingStatus
	outpoint           wire.OutPoint
}

func newClientHarness(t *testing.T) *clientHarness {
	t.Helper()
	db := loopdb.NewTestDB(t)
	r := testReservation()
	r.Terms = Terms{
		AssetID: r.AssetID,
		Amount:  r.Amount,
	}
	r.State = RequestQuote
	h := &clientHarness{
		t:     t,
		id:    r.ID,
		quote: testPurchaseQuote(r),
		store: &failingStore{
			Store: NewSqlStore(db.BaseDB),
		},
		preimage:    lntypes.Preimage{9},
		probeResult: ProbeSucceeded,
		outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{3},
			Index: 2,
		},
		status: FundingStatus{
			Height:             101,
			ConfirmationHeight: 100,
		},
	}
	h.cfg = &Config{
		Store:       h.store,
		Server:      h,
		Payments:    h,
		Wallet:      h,
		Clock:       clock.NewTestClock(time.Unix(1800000000, 0)),
		NotifyAdmin: func(ID, error) { h.notifications++ },
	}
	h.store.Store.(*SqlStore).clock = h.cfg.Clock
	require.NoError(t, h.store.CreateReservation(t.Context(), r))
	h.restart()
	return h
}

func (h *clientHarness) record() *Reservation {
	h.t.Helper()
	r, err := h.store.GetReservation(h.t.Context(), h.id)
	require.NoError(h.t, err)
	return r
}

func (h *clientHarness) restart() {
	h.t.Helper()
	var err error
	h.machine, err = NewFSM(h.cfg, h.record())
	require.NoError(h.t, err)
}

func (h *clientHarness) event(event fsm.EventType, data fsm.EventContext) {
	h.t.Helper()
	require.NoError(h.t, h.machine.SendEvent(h.t.Context(), event, data))
	for range 3 {
		if h.record().State != ProbeRoutes || h.machine.LastActionError != nil {
			break
		}
		require.NoError(h.t, h.machine.SendEvent(h.t.Context(), OnRecover, nil))
	}
}

func (h *clientHarness) state(state fsm.StateType) {
	h.t.Helper()
	require.Equal(h.t, state, h.record().State)
}

func (h *clientHarness) QuoteAssetReservation(ctx context.Context,
	r *swapserverrpc.QuoteAssetReservationRequest, _ ...grpc.CallOption) (
	*swapserverrpc.AssetReservation, error) {

	h.quotes++
	if h.quoteErr != nil {
		return nil, h.quoteErr
	}
	if h.blockQuote {
		select {
		case h.quoteStarted <- struct{}{}:

		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	require.Equal(h.t, h.id[:], r.ReservationId)
	if h.quotePending {
		return &swapserverrpc.AssetReservation{ReservationId: r.ReservationId}, nil
	}
	return &swapserverrpc.AssetReservation{
		ReservationId: h.quote.ReservationId,
		Quote:         proto.Clone(h.quote).(*swapserverrpc.AssetReservationQuote),
	}, nil
}

func (h *clientHarness) GetAssetReservation(context.Context,
	*swapserverrpc.AssetReservationSelector, ...grpc.CallOption) (
	*swapserverrpc.AssetReservation, error) {

	response := &swapserverrpc.AssetReservation{
		ReservationId: h.quote.ReservationId,
		Quote:         h.quote,
		State:         swapserverrpc.AssetReservationStatus_ASSET_RESERVATION_FUNDING,
		ProbeCanceled: h.probeCanceled, ProbeSucceeded: h.probeSucceeded,
	}
	if h.deliver {
		response.State = swapserverrpc.AssetReservationStatus_ASSET_RESERVATION_READY
		response.Outpoint = h.outpoint.String()
		response.ProofAvailable = true
		response.PrepayCredit = h.quote.Terms.Fee
	}
	return response, nil
}

func (h *clientHarness) GetAssetReservationProof(context.Context,
	*swapserverrpc.AssetReservationSelector, ...grpc.CallOption) (
	*swapserverrpc.AssetReservationProof, error) {

	return &swapserverrpc.AssetReservationProof{
		ReservationId: h.quote.ReservationId,
		Outpoint:      h.outpoint.String(),
		Proof:         []byte{1, 2, 3},
	}, nil
}

func (h *clientHarness) CancelAssetReservation(ctx context.Context,
	req *swapserverrpc.CancelAssetReservationRequest, _ ...grpc.CallOption) (
	*swapserverrpc.AssetReservation, error) {

	if req.ProbeOnly {
		h.probeCanceled = true
		return h.GetAssetReservation(ctx, req.Reservation)
	}
	h.cancels++
	return &swapserverrpc.AssetReservation{
		FundingUnavailable: h.fundingUnavailable,
		ReservationId:      h.quote.ReservationId,
		State:              swapserverrpc.AssetReservationStatus_ASSET_RESERVATION_CANCELED,
	}, nil
}

func (h *clientHarness) ValidateQuote(context.Context,
	*swapserverrpc.AssetReservationQuote) error {

	return h.quoteValidationErr
}

func (h *clientHarness) PrepareProbe(_ context.Context,
	r *Reservation) (*PaymentPlan, error) {

	return &PaymentPlan{
		Hash: ProbeHash(r.ID), NodeKey: r.ClientKey.PubKey,
		Request: &routerrpc.SendPaymentRequest{
			PaymentRequest: r.Quote.ProbeInvoice,
			TimeoutSeconds: 10, MaxParts: 1,
			CltvLimit: 160, FeeLimitMsat: 100,
		},
	}, nil
}

func (h *clientHarness) SendProbe(_ context.Context, r *Reservation,
	observe func(*lnrpc.Payment) error) error {
	h.probeInvoices = append(h.probeInvoices, r.Quote.ProbeInvoice)
	h.probeSucceeded = h.probeResult == ProbeSucceeded
	h.probeCanceled = true
	h.probePayment = &lnrpc.Payment{
		PaymentHash:   ProbeHash(r.ID).String(),
		Status:        lnrpc.Payment_FAILED,
		FailureReason: lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE,
	}
	if h.probeResult == ProbeTimedOut {
		h.probePayment.FailureReason =
			lnrpc.PaymentFailureReason_FAILURE_REASON_TIMEOUT
	}
	if h.probeSucceeded {
		// Default LND settings do not retain failed HTLC attempts.
		h.probePayment.FailureReason =
			lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS
	}
	return observe(&lnrpc.Payment{
		PaymentHash: ProbeHash(r.ID).String(),
		Status:      lnrpc.Payment_IN_FLIGHT,
		Htlcs: []*lnrpc.HTLCAttempt{{
			Status: lnrpc.HTLCAttempt_IN_FLIGHT,
			Route: &lnrpc.Route{
				TotalFeesMsat: 10,
				TotalAmtMsat:  int64(r.Quote.EstimatedMainAmountMsat) + 10,
			},
		}},
	})
}

func (h *clientHarness) TrackProbe(_ context.Context, _ *Reservation,
	observe func(*lnrpc.Payment) error) error {

	return observe(h.probePayment)
}

func (h *clientHarness) Prepare(_ context.Context,
	r *Reservation) (*PaymentPlan, error) {

	return &PaymentPlan{
		Hash:    h.preimage.Hash(),
		NodeKey: r.ClientKey.PubKey,
		Request: &routerrpc.SendPaymentRequest{
			PaymentRequest: r.Quote.PrepayInvoice,
			FeeLimitMsat:   int64(r.MaxRouteFeeMsat),
		},
	}, nil
}

func (h *clientHarness) Lookup(_ context.Context, _ *btcec.PublicKey,
	hash lntypes.Hash) (*lnrpc.Payment, error) {

	if hash == ProbeHash(ID(h.quote.ReservationId)) {
		if h.probePayment == nil {
			return nil, ErrPaymentNotFound
		}
		return proto.Clone(h.probePayment).(*lnrpc.Payment), nil
	}
	h.lookups++
	if h.result == nil {
		return nil, ErrPaymentNotFound
	}
	return proto.Clone(h.result).(*lnrpc.Payment), nil
}

func (h *clientHarness) Pay(context.Context, *btcec.PublicKey,
	*routerrpc.SendPaymentRequest) error {

	h.sends++
	h.result = &lnrpc.Payment{
		PaymentHash: h.preimage.Hash().String(),
		Status:      lnrpc.Payment_IN_FLIGHT,
	}
	return errors.New("lost send reply")
}

func (h *clientHarness) DeriveKey(context.Context) (
	*keychain.KeyDescriptor, error) {

	h.deriveCalls++
	key := testReservation().ClientKey
	return &key, nil
}

func (h *clientHarness) Verify(context.Context, *Reservation, []byte) (
	FundingStatus, error) {

	h.verifyCalls++
	if h.badProof {
		return FundingStatus{}, ErrInvalidReservation
	}
	return h.status, nil
}

func (h *clientHarness) Inspect(context.Context, *Reservation) (
	FundingStatus, error) {

	return h.status, nil
}

func (h *clientHarness) approve() {
	h.t.Helper()
	h.event(OnApprove, testApproval(h.t, h.record()))
}

func (h *clientHarness) settle() {
	h.result.Status = lnrpc.Payment_SUCCEEDED
	h.result.ValueMsat = int64(h.record().Quote.PrepayAmountMsat)
	h.result.PaymentPreimage = h.preimage.String()
}

func TestClientPurchaseFSM(t *testing.T) {
	h := newClientHarness(t)
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	h.restart()
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.Zero(t, h.sends)
	h.approve()
	h.state(PayPrepay)
	require.Equal(t, 1, h.sends)
	h.restart()
	h.event(OnRecover, nil)
	require.Equal(t, 1, h.sends)
	h.settle()
	h.event(OnRecover, nil)
	h.state(WaitForDelivery)
	h.deliver = true
	h.event(OnRecover, nil)
	h.state(VerifyReservation)
	require.Empty(t, h.record().ReservationProof)
	h.status.Height++
	h.event(OnRecover, nil)
	h.state(Ready)
	require.EqualValues(t, 11, h.record().PrepayCredit)
	require.EqualValues(t, 100, h.record().ConfirmationHeight)
	h.restart()
	h.status.Height = 1539
	h.event(OnRecover, nil)
	h.state(Ready)
	h.status.Height++
	h.event(OnRecover, nil)
	h.state(Expired)
	require.Equal(t, 1, h.sends)
	require.Zero(t, h.notifications)
}

func TestClientProbeApproval(t *testing.T) {
	h := newClientHarness(t)
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.EqualValues(t, 10, h.record().Probes.MainFeeMsat)
	require.EqualValues(t, 1, h.record().Probes.PrepayFeeMsat)
	h.restart()
	h.event(OnRecover, nil)
	require.Len(t, h.probeInvoices, 1)
	a := testApproval(t, h.record())
	a.QuoteHash[0] ^= 1
	h.event(OnApprove, a)
	h.state(AwaitApproval)
	require.Zero(t, h.sends)
	a.QuoteHash[0] ^= 1
	h.event(OnApprove, a)
	require.Equal(t, 1, h.sends)
}

func TestClientSkipProbeSurvivesRecovery(t *testing.T) {
	h := newClientHarness(t)
	r := h.record()
	r.SkipProbe = true
	require.NoError(t, h.store.UpdateReservation(t.Context(), r))
	h.restart()
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.Empty(t, h.probeInvoices)
	require.Equal(t, ProbeResults{}, h.record().Probes)

	// Recovery never overrides the saved skip preference.
	h.restart()
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.True(t, h.record().SkipProbe)
	require.Empty(t, h.probeInvoices)
	approval := testApproval(t, h.record())
	approval.SkipProbe = true
	h.event(OnApprove, approval)
	require.Equal(t, 1, h.sends)
	require.Empty(t, h.probeInvoices)
}

func TestClientFailedProbeCancelsPurchase(t *testing.T) {
	for _, outcome := range []ProbeOutcome{ProbeFailed, ProbeTimedOut} {
		t.Run(fmt.Sprint(outcome), func(t *testing.T) {
			h := newClientHarness(t)
			h.probeResult = outcome
			h.event(OnRecover, nil)
			h.state(Canceled)
			require.Equal(t, 1, h.cancels)
			require.Equal(t, outcome, h.record().Probes.Main)
			quote := proto.Clone(h.record().Quote)

			// Recovery retains this purchase and its failed probe. It
			// cannot create another quote or approve the canceled one.
			h.restart()
			h.event(OnRecover, nil)
			h.state(Canceled)
			require.Equal(t, 1, h.quotes)
			require.Len(t, h.probeInvoices, 1)
			require.True(t, proto.Equal(quote, h.record().Quote))
			approval := testApproval(t, h.record())
			approval.SkipProbe = true
			require.ErrorIs(t, h.machine.SendEvent(
				t.Context(), OnApprove, approval,
			), fsm.ErrEventRejected)
			require.Zero(t, h.sends)
		})
	}
}

func TestClientPurchaseWriteBoundaries(t *testing.T) {
	for _, boundary := range []string{"approval", "payment"} {
		for _, skipProbe := range []bool{false, true} {
			name := fmt.Sprintf("%s/skip_probe=%t", boundary, skipProbe)
			t.Run(name, func(t *testing.T) {
				h := newClientHarness(t)
				h.event(OnRecover, nil)
				h.store.fail = func(r *Reservation) bool {
					if boundary == "approval" {
						return r.State == PayPrepay
					}
					return r.PaymentRequest != nil
				}
				a := testApproval(t, h.record())
				a.SkipProbe = skipProbe
				if skipProbe {
					// Zero is a valid cap, not an approval marker.
					a.MaxRouteFeeMsat = 0
				}
				h.event(OnApprove, a)
				require.Zero(t, h.sends)
				h.store.fail = nil
				h.restart()
				if boundary == "approval" {
					h.state(AwaitApproval)
					require.Zero(t, h.record().MaxRouteFeeMsat)
					require.False(t, h.record().SkipProbe)
					h.event(OnRecover, nil)
					require.Zero(t, h.sends)
					h.event(OnApprove, a)
				} else {
					h.state(PayPrepay)
					h.event(OnRecover, nil)
				}
				require.Equal(t, 1, h.sends)
				require.Equal(t, a.MaxRouteFeeMsat,
					h.record().MaxRouteFeeMsat)
				require.Equal(t, a.SkipProbe, h.record().SkipProbe)
			})
		}
	}
}

func TestClientSavedPaymentChoicesCannotChange(t *testing.T) {
	h := newClientHarness(t)
	h.event(OnRecover, nil)
	// Commit approval but fail before saving the exact payment request.
	h.store.fail = func(r *Reservation) bool {
		return r.PaymentRequest != nil
	}
	h.approve()
	h.state(PayPrepay)
	require.Zero(t, h.sends)
	h.store.fail = nil
	h.restart()
	changed := testApproval(t, h.record())
	changed.MaxRouteFeeMsat++
	require.ErrorIs(t, h.machine.SendEvent(t.Context(), OnApprove, changed),
		fsm.ErrEventRejected)
	require.EqualValues(t, 100, h.record().MaxRouteFeeMsat)
	for _, mutate := range []func(*Reservation){
		func(r *Reservation) { r.MaxRouteFeeMsat++ },
		func(r *Reservation) { r.SkipProbe = true },
		func(r *Reservation) { r.State = AwaitApproval },
	} {
		r := h.record()
		mutate(r)
		require.ErrorIs(t, h.store.UpdateReservation(t.Context(), r),
			ErrRequestConflict)
	}
	h.event(OnRecover, nil)
	require.Equal(t, 1, h.sends)
}

func TestClientApprovalEventRequiresConsent(t *testing.T) {
	h := newClientHarness(t)
	h.event(OnRecover, nil)

	// Payment preferences alone do not authorize a purchase. Recovery must
	// wait until explicit approval saves the transition to PayPrepay.
	r := h.record()
	r.MaxRouteFeeMsat = 100
	r.SkipProbe = true
	require.NoError(t, h.store.UpdateReservation(t.Context(), r))
	h.restart()
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.Zero(t, h.sends)

	h.event(OnApproved, nil)
	require.Error(t, h.machine.LastActionError)
	h.state(AwaitApproval)
	require.Zero(t, h.sends)
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.Zero(t, h.sends)
}

func TestClientCanceledAndLateSettledPayment(t *testing.T) {
	for _, name := range []string{"failed", "settled"} {
		t.Run(name, func(t *testing.T) {
			settled := name == "settled"
			h := newClientHarness(t)
			h.event(OnRecover, nil)
			h.approve()
			h.event(OnCancel, nil)
			h.state(CancelPrepay)
			if settled {
				h.settle()
			} else {
				h.result.Status = lnrpc.Payment_FAILED
			}
			h.cfg.Clock.(*clock.TestClock).SetTime(time.Unix(2000000000, 0))
			h.restart()
			h.event(OnRecover, nil)
			if settled {
				h.state(WaitForDelivery)
			} else {
				h.state(Canceled)
			}
			require.Equal(t, 1, h.sends)
		})
	}
}

// TestClientRepeatedCancel accepts cancellation while one is already pending.
// A caller may cancel twice, or its cancel may race the quote expiry that
// already moved the purchase into CancelPrepay.
func TestClientRepeatedCancel(t *testing.T) {
	h := newClientHarness(t)
	h.event(OnRecover, nil)
	h.approve()
	h.event(OnCancel, nil)
	h.state(CancelPrepay)
	h.event(OnCancel, nil)
	h.state(CancelPrepay)
}

func TestClientInvalidProof(t *testing.T) {
	h := newClientHarness(t)
	h.event(OnRecover, nil)
	h.approve()
	h.settle()
	h.deliver, h.badProof = true, true
	h.event(OnRecover, nil)
	h.state(NeedAdminAttention)
	require.Empty(t, h.record().ReservationProof)
	require.Equal(t, 1, h.notifications)
	for name, state := range h.machine.states() {
		require.Equal(t, name, state.Transitions[OnRecover])
	}
}

// TestClientQuoteRejected never retries a definitive funding refusal, including
// after restart. No quote, probe or payment exists for this purchase.
func TestClientQuoteRejected(t *testing.T) {
	h := newClientHarness(t)
	h.quoteErr = status.Error(
		codes.OutOfRange, "amount above current maximum",
	)
	h.event(OnRecover, nil)
	h.state(QuoteRejected)
	require.True(t, IsFinal(h.record().State))
	require.Nil(t, h.record().Quote)
	require.Zero(t, h.sends)
	require.Empty(t, h.probeInvoices)
	require.Zero(t, h.cancels)
	h.quoteErr = nil
	h.restart()
	h.event(OnRecover, nil)
	h.state(QuoteRejected)
	require.Equal(t, 1, h.quotes)
}

// TestClientQuoteUnavailableStillRetries preserves recovery of uncertain RPCs.
func TestClientQuoteUnavailableStillRetries(t *testing.T) {
	h := newClientHarness(t)
	h.quoteErr = status.Error(codes.Unavailable, "reservation unavailable")
	h.event(OnRecover, nil)
	h.state(RequestQuote)
	require.EqualError(t, h.machine.LastActionError, h.quoteErr.Error())
	h.quoteErr = nil
	h.restart()
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.Equal(t, 2, h.quotes)
}

// TestClientHeldFundingRefusal waits for local payment resolution before
// persisting the public funding error. A held payment is never resent.
func TestClientHeldFundingRefusal(t *testing.T) {
	h := newClientHarness(t)
	h.event(OnRecover, nil)
	h.approve()
	h.fundingUnavailable = true
	h.event(OnCancel, nil)
	h.state(CancelPrepay)
	h.restart()
	h.event(OnRecover, nil)
	h.state(CancelPrepay)
	require.Equal(t, 1, h.sends)
	h.result.Status = lnrpc.Payment_FAILED
	h.result.FailureReason =
		lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS
	h.event(OnRecover, nil)
	h.state(QuoteRejected)
	require.True(t, IsFinal(h.record().State))
	h.restart()
	h.event(OnRecover, nil)
	h.state(QuoteRejected)
	require.Equal(t, 1, h.sends)
}

func TestClientFundingRefusalCannotOverrideSettlement(t *testing.T) {
	h := newClientHarness(t)
	h.event(OnRecover, nil)
	h.approve()
	h.fundingUnavailable = true
	h.event(OnCancel, nil)
	h.state(CancelPrepay)
	h.settle()
	h.event(OnRecover, nil)
	h.state(WaitForDelivery)
}

func TestReadyVerifiesOncePerRestart(t *testing.T) {
	h := newClientHarness(t)
	h.event(OnRecover, nil)
	h.approve()
	h.settle()
	h.deliver = true
	h.status.Height = 102
	h.event(OnRecover, nil)
	h.state(Ready)
	calls := h.verifyCalls
	for range 10 {
		h.event(OnRecover, nil)
	}
	require.Equal(t, calls, h.verifyCalls)
	h.restart()
	h.event(OnRecover, nil)
	require.Equal(t, calls+1, h.verifyCalls)
	for range 10 {
		h.event(OnRecover, nil)
	}
	require.Equal(t, calls+1, h.verifyCalls)
	// A restarted process must not trust persisted proof without verification.
	h.restart()
	h.badProof = true
	h.status.Height = 1540
	h.event(OnRecover, nil)
	h.state(Ready)
	require.ErrorIs(t, h.machine.LastActionError, ErrInvalidReservation)
}

func TestQuoteFailuresStopBeforePayment(t *testing.T) {
	for _, failure := range []string{"invalid", "pending", "unavailable"} {
		t.Run(failure, func(t *testing.T) {
			h := newClientHarness(t)
			switch failure {
			case "invalid":
				h.quoteValidationErr = errors.New("invalid invoice")
			case "pending":
				h.quotePending = true
			case "unavailable":
				h.quoteErr = errors.New("server offline")
			}
			h.event(OnRecover, nil)
			if failure != "invalid" {
				h.state(RequestQuote)
				h.cfg.Clock.(*clock.TestClock).SetTime(
					h.record().CreatedAt.Add(time.Minute),
				)
				h.restart()
				h.event(OnRecover, nil)
			}
			h.state(QuoteFailed)
			calls := h.quotes
			h.restart()
			h.event(OnRecover, nil)
			require.Equal(t, calls, h.quotes)
			require.Zero(t, h.sends)
			require.Nil(t, h.record().ProbeRequest)
			active, err := h.store.GetReservations(t.Context(),
				StateFilter{ActiveOnly: true})
			require.NoError(t, err)
			require.Empty(t, active)
		})
	}
}
