package reservation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestProbeRecoveryRecognizesCompletedPayment(t *testing.T) {
	for _, quoteExpired := range []bool{false, true} {
		name := "valid quote"
		if quoteExpired {
			name = "expired quote"
		}
		t.Run(name, func(t *testing.T) {
			h := newClientHarness(t)
			// The receiver completed the probe, but the client stopped
			// before recording the terminal observation.
			require.NoError(t, h.machine.SendEvent(
				t.Context(), OnRecover, nil,
			))
			h.state(ProbeRoutes)
			require.Nil(t, h.record().ProbeResult)
			now := h.record().ProbeDeadline.Add(time.Second)
			if quoteExpired {
				now = time.Unix(h.quote.ExpiresAt, 0)
			}
			h.cfg.Clock.(*clock.TestClock).SetTime(now)
			h.restart()
			h.event(OnRecover, nil)
			if quoteExpired {
				h.state(Canceled)
				require.Equal(t, 1, h.cancels)
			} else {
				h.state(AwaitApproval)
				require.Equal(t, ProbeSucceeded, h.record().Probes.Main)
				require.Zero(t, h.cancels)
			}
			require.Len(t, h.probeInvoices, 1)
			require.Zero(t, h.sends)
		})
	}
}

type delayedProbeStatus struct{ *clientHarness }

func (s delayedProbeStatus) GetAssetReservation(ctx context.Context,
	req *swapserverrpc.AssetReservationSelector, opts ...grpc.CallOption) (
	*swapserverrpc.AssetReservation, error) {

	s.cfg.Clock.(*clock.TestClock).SetTime(s.record().ProbeDeadline)
	return s.clientHarness.GetAssetReservation(ctx, req, opts...)
}

func TestProbeDoesNotDispatchAfterSlowServerLookup(t *testing.T) {
	h := newClientHarness(t)
	h.cfg.Server = delayedProbeStatus{clientHarness: h}
	h.restart()
	h.event(OnRecover, nil)
	h.state(Canceled)
	require.Equal(t, ProbeTimedOut, h.record().Probes.Main)
	require.Equal(t, 1, h.cancels)
	require.Empty(t, h.probeInvoices)
}

func TestProbeResumesPaymentWithoutResending(t *testing.T) {
	h := newClientHarness(t)
	// Stop after dispatch, before the result has been read or persisted.
	require.NoError(t, h.machine.SendEvent(t.Context(), OnRecover, nil))
	h.state(ProbeRoutes)
	require.Len(t, h.probeInvoices, 1)
	h.probePayment.Status = lnrpc.Payment_IN_FLIGHT
	h.probeCanceled = false
	h.restart()
	require.NoError(t, h.machine.SendEvent(t.Context(), OnRecover, nil))
	h.state(ProbeRoutes)
	require.Len(t, h.probeInvoices, 1)

	// Expiration requests remote cancellation, but cannot finish while the
	// local HTLC remains in flight. A restart must still only track it.
	h.cfg.Clock.(*clock.TestClock).SetTime(
		h.record().ProbeDeadline.Add(time.Second),
	)
	h.event(OnRecover, nil)
	require.Equal(t, 1, h.cancels)
	h.state(CancelPrepay)
	h.restart()
	h.probePayment.Status = lnrpc.Payment_FAILED
	h.probeSucceeded = false
	h.event(OnRecover, nil)
	h.state(Canceled)
	require.Equal(t, ProbeTimedOut, h.record().Probes.Main)
	require.Len(t, h.probeInvoices, 1)
}

func TestProbeRequestMustBeDurableBeforeSend(t *testing.T) {
	h := newClientHarness(t)
	h.store.fail = func(r *Reservation) bool { return r.ProbeRequest != nil }
	require.NoError(t, h.machine.SendEvent(t.Context(), OnRecover, nil))
	require.Empty(t, h.probeInvoices)
	require.Nil(t, h.record().ProbeRequest)
	h.store.fail = nil
	h.restart()
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.Len(t, h.probeInvoices, 1)
}

func TestProbeFailureRequiresReceiverEvidence(t *testing.T) {
	for _, mismatch := range []string{"receipt", "reason"} {
		t.Run(mismatch, func(t *testing.T) {
			h := newClientHarness(t)
			require.NoError(t, h.machine.SendEvent(t.Context(), OnRecover, nil))
			switch mismatch {
			case "receipt":
				h.probeSucceeded = false
			case "reason":
				h.probePayment.FailureReason =
					lnrpc.PaymentFailureReason_FAILURE_REASON_NO_ROUTE
			}
			h.event(OnRecover, nil)
			h.state(Canceled)
			require.Equal(t, 1, h.cancels)
			h.restart()
			h.event(OnRecover, nil)
			require.Len(t, h.probeInvoices, 1)
		})
	}
}

// Success and its fee estimate survive restart without retained attempts.
func TestProbeSucceedsWithoutHTLCAttempts(t *testing.T) {
	h := newClientHarness(t)
	require.NoError(t, h.machine.SendEvent(t.Context(), OnRecover, nil))
	require.Empty(t, h.probePayment.Htlcs)
	require.EqualValues(t, 10, h.record().Probes.MainFeeMsat)
	require.True(t, h.record().Probes.FeeKnown)

	// Save the terminal payment, then stop before saving probe success.
	h.store.fail = func(r *Reservation) bool {
		return !r.Probes.CheckedAt.IsZero()
	}
	require.NoError(t, h.machine.SendEvent(t.Context(), OnRecover, nil))
	require.NotNil(t, h.record().ProbeResult)
	require.Empty(t, h.record().ProbeResult.Htlcs)
	h.store.fail = nil
	h.restart()
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.Equal(t, ProbeSucceeded, h.record().Probes.Main)
	require.EqualValues(t, 10, h.record().Probes.MainFeeMsat)
	require.True(t, h.record().Probes.FeeKnown)
	require.EqualValues(t, 1, h.record().Probes.PrepayFeeMsat)
	require.Len(t, h.probeInvoices, 1)
	require.Zero(t, h.cancels)
}

func TestProbeLiveFeeWriteFailureStopsObservation(t *testing.T) {
	h := newClientHarness(t)
	h.store.fail = func(r *Reservation) bool { return r.Probes.FeeKnown }
	require.NoError(t, h.machine.SendEvent(t.Context(), OnRecover, nil))
	require.False(t, h.record().Probes.FeeKnown)
	require.Nil(t, h.record().ProbeResult)
	require.Len(t, h.probeInvoices, 1)
	require.Zero(t, h.cancels)

	// The payment resolved while the route write was unavailable. Recovery
	// can recognize delivery, but must not invent a fee from its result.
	h.store.fail = nil
	h.restart()
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.False(t, h.record().Probes.FeeKnown)
	require.Len(t, h.probeInvoices, 1)
}

func TestProbeRecoveryWithoutRouteStillSucceeds(t *testing.T) {
	h := newClientHarness(t)
	require.NoError(t, h.machine.SendEvent(t.Context(), OnRecover, nil))
	// Model a restart that missed the live route before LND pruned it.
	r := h.record()
	r.Probes.MainFeeMsat, r.Probes.FeeKnown = 0, false
	require.NoError(t, h.store.UpdateReservation(t.Context(), r))
	h.restart()
	h.event(OnRecover, nil)
	h.state(AwaitApproval)
	require.False(t, h.record().Probes.FeeKnown)
	require.Equal(t, ProbeSucceeded, h.record().Probes.Main)
	require.Zero(t, h.cancels)
}

type probePreparationFailure struct{ Payments }

func (p probePreparationFailure) PrepareProbe(context.Context,
	*Reservation) (*PaymentPlan, error) {

	return nil, errors.New("paying node unavailable")
}

func TestProbePreparationFailureCancelsPurchase(t *testing.T) {
	h := newClientHarness(t)
	h.cfg.Payments = probePreparationFailure{Payments: h}
	h.restart()
	h.event(OnRecover, nil)
	h.state(Canceled)
	require.Equal(t, ProbeFailed, h.record().Probes.Main)
	require.Equal(t, 1, h.cancels)
	require.Empty(t, h.probeInvoices)
	require.Zero(t, h.sends)
}

type probeTrackingFailure struct{ Payments }

func (p probeTrackingFailure) Lookup(context.Context, *btcec.PublicKey,
	lntypes.Hash) (*lnrpc.Payment, error) {

	return nil, errors.New("paying node unavailable")
}

func TestProbeTrackingFailureStillRequestsCancellation(t *testing.T) {
	h := newClientHarness(t)
	require.NoError(t, h.machine.SendEvent(t.Context(), OnRecover, nil))
	h.probePayment.Status = lnrpc.Payment_IN_FLIGHT
	h.cfg.Payments = probeTrackingFailure{Payments: h}
	h.restart()
	h.event(OnRecover, nil)
	h.state(CancelPrepay)
	require.Equal(t, 1, h.cancels)
	require.Equal(t, ProbeFailed, h.record().Probes.Main)
	require.Len(t, h.probeInvoices, 1)

	// Neither a tracking error nor a restart proves the HTLC resolved.
	h.restart()
	h.event(OnRecover, nil)
	h.state(CancelPrepay)
	h.cfg.Payments = h
	h.restart()
	h.event(OnRecover, nil)
	h.state(CancelPrepay)
	h.probePayment.Status = lnrpc.Payment_FAILED
	h.event(OnRecover, nil)
	h.state(Canceled)
	require.Len(t, h.probeInvoices, 1)
	require.Zero(t, h.sends)
}
