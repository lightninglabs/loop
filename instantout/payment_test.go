package instantout

import (
	"context"
	"testing"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type paymentTestRouter struct {
	lndclient.RouterClient

	statusChan chan lndclient.PaymentStatus
	errorChan  chan error
}

func (r *paymentTestRouter) SendPayment(context.Context,
	lndclient.SendPaymentRequest) (chan lndclient.PaymentStatus, chan error,
	error) {

	return r.statusChan, r.errorChan, nil
}

type paymentTestReservationManager struct {
	ReservationManager
}

func (m *paymentTestReservationManager) LockReservation(context.Context,
	reservation.ID) error {

	return nil
}

func (m *paymentTestReservationManager) UnlockReservation(context.Context,
	reservation.ID) error {

	return nil
}

type paymentTestInstantOutClient struct {
	swapserverrpc.InstantSwapServerClient

	accepted bool
	polls    int
}

func (c *paymentTestInstantOutClient) PollPaymentAccepted(context.Context,
	*swapserverrpc.PollPaymentAcceptedRequest, ...grpc.CallOption) (
	*swapserverrpc.PollPaymentAcceptedResponse, error) {

	c.polls++
	return &swapserverrpc.PollPaymentAcceptedResponse{
		Accepted: c.accepted,
	}, nil
}

func (c *paymentTestInstantOutClient) CancelInstantSwap(context.Context,
	*swapserverrpc.CancelInstantSwapRequest, ...grpc.CallOption) (
	*swapserverrpc.CancelInstantSwapResponse, error) {

	return &swapserverrpc.CancelInstantSwapResponse{}, nil
}

func newPaymentTestFSM(router lndclient.RouterClient,
	client swapserverrpc.InstantSwapServerClient) *FSM {

	return &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			RouterClient:       router,
			InstantOutClient:   client,
			ReservationManager: &paymentTestReservationManager{},
		},
		InstantOut: &InstantOut{
			SwapHash: lntypes.Hash{1},
			Reservations: []*reservation.Reservation{
				{ID: reservation.ID{1}},
			},
		},
	}
}

// TestPollPaymentAcceptedIgnoresClosedStreams verifies that the normal router
// end-of-stream signal doesn't race with the server's acceptance response.
func TestPollPaymentAcceptedIgnoresClosedStreams(t *testing.T) {
	statusChan := make(chan lndclient.PaymentStatus)
	errorChan := make(chan error)
	close(statusChan)
	close(errorChan)

	router := &paymentTestRouter{
		statusChan: statusChan,
		errorChan:  errorChan,
	}
	client := &paymentTestInstantOutClient{accepted: true}
	instantOutFSM := newPaymentTestFSM(router, client)

	event := instantOutFSM.PollPaymentAcceptedAction(t.Context(), nil)

	require.Equal(t, OnPaymentAccepted, event)
	require.Equal(t, 1, client.polls)
}

// TestPollPaymentAcceptedRejectsNilError verifies that an invalid nil error
// value can't make the FSM report a successful failure path.
func TestPollPaymentAcceptedRejectsNilError(t *testing.T) {
	errorChan := make(chan error, 1)
	errorChan <- nil

	router := &paymentTestRouter{
		statusChan: make(chan lndclient.PaymentStatus),
		errorChan:  errorChan,
	}
	instantOutFSM := newPaymentTestFSM(
		router, &paymentTestInstantOutClient{},
	)

	event := instantOutFSM.PollPaymentAcceptedAction(t.Context(), nil)

	require.Equal(t, fsm.OnError, event)
	require.ErrorContains(
		t, instantOutFSM.LastActionError,
		"payment error channel returned nil",
	)
}

// TestPollPaymentAcceptedPreservesContextError verifies that cancellation is
// recorded as the action error rather than becoming a nil FSM error.
func TestPollPaymentAcceptedPreservesContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	router := &paymentTestRouter{}
	instantOutFSM := newPaymentTestFSM(
		router, &paymentTestInstantOutClient{},
	)

	event := instantOutFSM.PollPaymentAcceptedAction(ctx, nil)

	require.Equal(t, fsm.OnError, event)
	require.ErrorIs(t, instantOutFSM.LastActionError, context.Canceled)
}
