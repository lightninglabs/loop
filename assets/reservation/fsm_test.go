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

	t             *testing.T
	store         *failingStore
	cfg           *Config
	machine       *FSM
	id            ID
	quote         *swapserverrpc.AssetReservationQuote
	preimage      lntypes.Preimage
	result        *lnrpc.Payment
	probeInvoices []string
	probeResult   ProbeOutcome
	sends         int
	quotes        int
	cancels       int
	lookups       int
	notifications int
	deriveCalls   int
	blockQuote    bool
	quoteStarted  chan struct{}
	deliver       bool
	badProof      bool
	status        FundingStatus
	outpoint      wire.OutPoint
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
}

func (h *clientHarness) state(state fsm.StateType) {
	h.t.Helper()
	require.Equal(h.t, state, h.record().State)
}

func (h *clientHarness) QuoteAssetReservation(ctx context.Context,
	r *swapserverrpc.QuoteAssetReservationRequest, _ ...grpc.CallOption) (
	*swapserverrpc.AssetReservation, error) {

	h.quotes++
	if h.blockQuote {
		select {
		case h.quoteStarted <- struct{}{}:

		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	require.Equal(h.t, h.id[:], r.ReservationId)
	return &swapserverrpc.AssetReservation{
		ReservationId: h.id[:],
		Quote:         proto.Clone(h.quote).(*swapserverrpc.AssetReservationQuote),
	}, nil
}

func (h *clientHarness) GetAssetReservation(context.Context,
	*swapserverrpc.AssetReservationSelector, ...grpc.CallOption) (
	*swapserverrpc.AssetReservation, error) {

	response := &swapserverrpc.AssetReservation{
		ReservationId: h.id[:],
		Quote:         h.quote,
		State:         swapserverrpc.AssetReservationStatus_ASSET_RESERVATION_FUNDING,
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
		ReservationId: h.id[:],
		Outpoint:      h.outpoint.String(),
		Proof:         []byte{1, 2, 3},
	}, nil
}

func (h *clientHarness) CancelAssetReservation(context.Context,
	*swapserverrpc.AssetReservationSelector, ...grpc.CallOption) (
	*swapserverrpc.AssetReservation, error) {

	h.cancels++
	return &swapserverrpc.AssetReservation{
		ReservationId: h.id[:],
		State:         swapserverrpc.AssetReservationStatus_ASSET_RESERVATION_CANCELED,
	}, nil
}

func (h *clientHarness) ValidateQuote(context.Context,
	*swapserverrpc.AssetReservationQuote) error {

	return nil
}

func (h *clientHarness) Probe(_ context.Context, invoice string) (
	ProbeOutcome, uint64, error) {

	h.probeInvoices = append(h.probeInvoices, invoice)
	return h.probeResult, 10, nil
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

func (h *clientHarness) Lookup(context.Context, *btcec.PublicKey,
	lntypes.Hash) (*lnrpc.Payment, error) {

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
	for _, outcome := range []ProbeOutcome{
		ProbeSucceeded, ProbeFailed, ProbeTimedOut, ProbeUnsupported,
	} {
		t.Run(fmt.Sprint(outcome), func(t *testing.T) {
			h := newClientHarness(t)
			h.probeResult = outcome
			h.event(OnRecover, nil)
			h.state(AwaitApproval)
			require.Equal(t, []string{h.quote.ProbeInvoice}, h.probeInvoices)
			r := h.record()
			require.Equal(t, outcome, r.Probes.Main)
			if outcome == ProbeSucceeded {
				require.EqualValues(t, 10, r.Probes.MainFeeMsat)
				require.EqualValues(t, 1, r.Probes.PrepayFeeMsat)
			} else {
				require.Zero(t, r.Probes.MainFeeMsat)
				require.Zero(t, r.Probes.PrepayFeeMsat)
			}
			// Saved probe outcomes and estimates survive recovery, and
			// resuming approval must not issue another probe.
			h.restart()
			h.event(OnRecover, nil)
			require.Equal(t, r.Probes, h.record().Probes)
			require.Len(t, h.probeInvoices, 1)

			a := testApproval(t, h.record())
			a.QuoteHash[0] ^= 1
			h.event(OnApprove, a)
			h.state(AwaitApproval)
			require.Zero(t, h.sends)
			a.QuoteHash[0] ^= 1
			h.event(OnApprove, a)
			if outcome != ProbeSucceeded {
				h.state(AwaitApproval)
				require.Error(t, h.machine.LastActionError)
				require.Zero(t, h.sends)
				require.Zero(t, h.record().MaxRouteFeeMsat)
				require.False(t, h.record().SkipProbe)
				a.SkipProbe = true
				h.event(OnApprove, a)
			}
			require.Equal(t, 1, h.sends)
			require.Len(t, h.probeInvoices, 1)
		})
	}
}

func TestClientExplicitProbeAfterSkipping(t *testing.T) {
	h := newClientHarness(t)
	r := h.record()
	r.SkipProbe = true
	require.NoError(t, h.store.UpdateReservation(t.Context(), r))
	h.restart()
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.Empty(t, h.probeInvoices)
	require.Equal(t, ProbeResults{}, h.record().Probes)

	// An explicit retry overrides the saved skip preference. Recovery
	// retains the real result and permits normal approval.
	h.event(OnProbeAgain, nil)
	h.state(AwaitApproval)
	require.False(t, h.record().SkipProbe)
	require.Equal(t, []string{h.quote.ProbeInvoice}, h.probeInvoices)
	h.restart()
	h.event(OnRecover, nil)
	h.approve()
	require.Equal(t, 1, h.sends)
	require.Len(t, h.probeInvoices, 1)
}

func TestClientPurchaseWriteBoundaries(t *testing.T) {
	for _, boundary := range []string{"approval", "payment"} {
		for _, skipProbe := range []bool{false, true} {
			name := fmt.Sprintf("%s/skip_probe=%t", boundary, skipProbe)
			t.Run(name, func(t *testing.T) {
				h := newClientHarness(t)
				if skipProbe {
					h.probeResult = ProbeFailed
				}
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
