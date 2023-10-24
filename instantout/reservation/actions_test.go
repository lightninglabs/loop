package reservation

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

var (
	defaultPubkeyBytes, _ = hex.DecodeString("021c97a90a411ff2b10dc2a8e32de2f29d2fa49d41bfbb52bd416e460db0747d0d")
	defaultPubkey, _      = btcec.ParsePubKey(defaultPubkeyBytes)

	defaultValue = btcutil.Amount(100)

	defaultExpiry = uint32(100)
)

func newValidInitReservationContext() *InitReservationContext {
	return &InitReservationContext{
		reservationID: ID{0x01},
		serverPubkey:  defaultPubkey,
		value:         defaultValue,
		expiry:        defaultExpiry,
		heightHint:    0,
	}
}

func newValidClientReturn() *swapserverrpc.ServerOpenReservationResponse {
	return &swapserverrpc.ServerOpenReservationResponse{}
}

type mockReservationClient struct {
	mock.Mock
}

func (m *mockReservationClient) OpenReservation(ctx context.Context,
	in *swapserverrpc.ServerOpenReservationRequest,
	opts ...grpc.CallOption) (*swapserverrpc.ServerOpenReservationResponse,
	error) {

	args := m.Called(ctx, in, opts)
	return args.Get(0).(*swapserverrpc.ServerOpenReservationResponse),
		args.Error(1)
}

func (m *mockReservationClient) ReservationNotificationStream(
	ctx context.Context, in *swapserverrpc.ReservationNotificationRequest,
	opts ...grpc.CallOption,
) (swapserverrpc.ReservationService_ReservationNotificationStreamClient,
	error) {

	args := m.Called(ctx, in, opts)
	return args.Get(0).(swapserverrpc.ReservationService_ReservationNotificationStreamClient),
		args.Error(1)
}

func (m *mockReservationClient) FetchL402(ctx context.Context,
	in *swapserverrpc.FetchL402Request,
	opts ...grpc.CallOption) (*swapserverrpc.FetchL402Response, error) {

	args := m.Called(ctx, in, opts)

	return args.Get(0).(*swapserverrpc.FetchL402Response),
		args.Error(1)
}

type mockStore struct {
	mock.Mock

	Store
}

func (m *mockStore) CreateReservation(ctx context.Context,
	reservation *Reservation) error {

	args := m.Called(ctx, reservation)
	return args.Error(0)
}

// TestInitReservationAction tests the InitReservationAction of the reservation
// state machine.
func TestInitReservationAction(t *testing.T) {
	tests := []struct {
		name             string
		eventCtx         fsm.EventContext
		mockStoreErr     error
		mockClientReturn *swapserverrpc.ServerOpenReservationResponse
		mockClientErr    error
		expectedEvent    fsm.EventType
	}{
		{
			name:             "success",
			eventCtx:         newValidInitReservationContext(),
			mockClientReturn: newValidClientReturn(),
			expectedEvent:    OnBroadcast,
		},
		{
			name:          "invalid context",
			eventCtx:      struct{}{},
			expectedEvent: fsm.OnError,
		},
		{
			name:          "reservation server error",
			eventCtx:      newValidInitReservationContext(),
			mockClientErr: errors.New("reservation server error"),
			expectedEvent: fsm.OnError,
		},
		{
			name:             "store error",
			eventCtx:         newValidInitReservationContext(),
			mockClientReturn: newValidClientReturn(),
			mockStoreErr:     errors.New("store error"),
			expectedEvent:    fsm.OnError,
		},
	}

	for _, tc := range tests {
		ctxb := context.Background()
		mockLnd := test.NewMockLnd()
		mockReservationClient := new(mockReservationClient)
		mockReservationClient.On(
			"OpenReservation", mock.Anything,
			mock.Anything, mock.Anything,
		).Return(tc.mockClientReturn, tc.mockClientErr)

		mockStore := new(mockStore)
		mockStore.On(
			"CreateReservation", mock.Anything, mock.Anything,
		).Return(tc.mockStoreErr)

		reservationFSM := &FSM{
			ctx: ctxb,
			cfg: &Config{
				Wallet:            mockLnd.WalletKit,
				ChainNotifier:     mockLnd.ChainNotifier,
				ReservationClient: mockReservationClient,
				Store:             mockStore,
			},
			StateMachine: &fsm.StateMachine{},
		}

		event := reservationFSM.InitAction(tc.eventCtx)
		require.Equal(t, tc.expectedEvent, event)
	}
}
