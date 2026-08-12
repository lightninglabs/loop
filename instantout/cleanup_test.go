package instantout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type cleanupTestReservationManager struct {
	ReservationManager

	unlockErr error
}

func (m *cleanupTestReservationManager) UnlockReservation(context.Context,
	reservation.ID) error {

	return m.unlockErr
}

type cleanupTestInstantOutClient struct {
	swapserverrpc.InstantSwapServerClient

	canceled chan struct{}
}

func (c *cleanupTestInstantOutClient) CancelInstantSwap(context.Context,
	*swapserverrpc.CancelInstantSwapRequest, ...grpc.CallOption) (
	*swapserverrpc.CancelInstantSwapResponse, error) {

	close(c.canceled)
	return &swapserverrpc.CancelInstantSwapResponse{}, nil
}

// TestCleanupPreservesActionError verifies that an unlock failure doesn't
// replace the action failure or prevent the cancellation notification.
func TestCleanupPreservesActionError(t *testing.T) {
	actionErr := errors.New("action failed")
	cancelClient := &cleanupTestInstantOutClient{
		canceled: make(chan struct{}),
	}
	instantOutFSM := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			ReservationManager: &cleanupTestReservationManager{
				unlockErr: errors.New("unlock failed"),
			},
			InstantOutClient: cancelClient,
		},
		InstantOut: &InstantOut{
			Reservations: []*reservation.Reservation{
				{ID: reservation.ID{1}},
			},
		},
	}

	event := instantOutFSM.handleErrorAndUnlockReservations(
		t.Context(), actionErr,
	)
	require.Equal(t, fsm.OnError, event)
	require.ErrorIs(t, instantOutFSM.LastActionError, actionErr)
	require.Eventually(t, func() bool {
		select {
		case <-cancelClient.canceled:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
}
