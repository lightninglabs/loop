package reservation

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
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

type MockChainNotifier struct {
	mock.Mock
}

func (m *MockChainNotifier) RegisterConfirmationsNtfn(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs, heightHint int32,
	options ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

	args := m.Called(ctx, txid, pkScript, numConfs, heightHint)
	return args.Get(0).(chan *chainntnfs.TxConfirmation), args.Get(1).(chan error), args.Error(2)
}

func (m *MockChainNotifier) RegisterBlockEpochNtfn(ctx context.Context) (
	chan int32, chan error, error) {

	args := m.Called(ctx)
	return args.Get(0).(chan int32), args.Get(1).(chan error), args.Error(2)
}

func (m *MockChainNotifier) RegisterSpendNtfn(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint int32) (
	chan *chainntnfs.SpendDetail, chan error, error) {

	args := m.Called(ctx, pkScript, heightHint)
	return args.Get(0).(chan *chainntnfs.SpendDetail), args.Get(1).(chan error), args.Error(2)
}

// TestSubscribeToConfirmationAction tests the SubscribeToConfirmationAction of
// the reservation state machine.
func TestSubscribeToConfirmationAction(t *testing.T) {
	tests := []struct {
		name          string
		blockHeight   int32
		blockErr      error
		sendTxConf    bool
		confErr       error
		expectedEvent fsm.EventType
	}{
		{
			name:          "success",
			blockHeight:   0,
			sendTxConf:    true,
			expectedEvent: OnConfirmed,
		},
		{
			name:          "expired",
			blockHeight:   100,
			expectedEvent: OnTimedOut,
		},
		{
			name:          "block error",
			blockHeight:   0,
			blockErr:      errors.New("block error"),
			expectedEvent: fsm.OnError,
		},
		{
			name:          "tx confirmation error",
			blockHeight:   0,
			confErr:       errors.New("tx confirmation error"),
			expectedEvent: fsm.OnError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { // Create a mock ChainNotifier and Reservation
			chainNotifier := new(MockChainNotifier)

			// Define your FSM
			r := NewFSMFromReservation(
				context.Background(), &Config{
					ChainNotifier: chainNotifier,
				},
				&Reservation{
					Expiry:       defaultExpiry,
					ServerPubkey: defaultPubkey,
					ClientPubkey: defaultPubkey,
					Value:        defaultValue,
				},
			)
			pkScript, err := r.reservation.GetPkScript()
			require.NoError(t, err)

			confChan := make(chan *chainntnfs.TxConfirmation)
			confErrChan := make(chan error)
			blockChan := make(chan int32)
			blockErrChan := make(chan error)

			// Define the expected return values for your mocks
			chainNotifier.On(
				"RegisterConfirmationsNtfn", mock.Anything, mock.Anything,
				mock.Anything, mock.Anything, mock.Anything,
			).Return(confChan, confErrChan, nil)

			chainNotifier.On("RegisterBlockEpochNtfn", mock.Anything).Return(
				blockChan, blockErrChan, nil,
			)

			go func() {
				// Send the tx confirmation.
				if tc.sendTxConf {
					confChan <- &chainntnfs.TxConfirmation{
						Tx: &wire.MsgTx{
							TxIn: []*wire.TxIn{},
							TxOut: []*wire.TxOut{
								{
									Value:    int64(defaultValue),
									PkScript: pkScript,
								},
							},
						},
					}
				}
			}()

			go func() {
				// Send the block notification.
				if tc.blockHeight != 0 {
					blockChan <- tc.blockHeight
				}
			}()

			go func() {
				// Send the block notification error.
				if tc.blockErr != nil {
					blockErrChan <- tc.blockErr
				}
			}()

			go func() {
				// Send the tx confirmation error.
				if tc.confErr != nil {
					confErrChan <- tc.confErr
				}
			}()

			eventType := r.SubscribeToConfirmationAction(nil)
			// Assert that the return value is as expected
			require.Equal(t, tc.expectedEvent, eventType)

			// Assert that the expected functions were called on the mocks
			chainNotifier.AssertExpectations(t)
		})
	}
}

// AsyncWaitForExpiredOrSweptAction tests the AsyncWaitForExpiredOrSweptAction
// of the reservation state machine.
func TestAsyncWaitForExpiredOrSweptAction(t *testing.T) {
	tests := []struct {
		name          string
		blockHeight   int32
		blockErr      error
		spendDetail   *chainntnfs.SpendDetail
		expectedState fsm.StateType
	}{
		{
			name:          "expired",
			blockHeight:   100,
			expectedState: TimedOut,
		},
		{
			name:          "block error",
			blockHeight:   0,
			blockErr:      errors.New("block error"),
			expectedState: Confirmed,
		},
		{
			name:          "spent",
			expectedState: Spent,
			spendDetail:   &chainntnfs.SpendDetail{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { // Create a mock ChainNotifier and Reservation
			chainNotifier := new(MockChainNotifier)

			// Define your FSM
			r := NewFSMFromReservation(
				context.Background(), &Config{
					ChainNotifier: chainNotifier,
				},
				&Reservation{
					ServerPubkey: defaultPubkey,
					ClientPubkey: defaultPubkey,
					Expiry:       defaultExpiry,
				},
			)

			blockChan := make(chan int32)
			blockErrChan := make(chan error)

			// Define the expected return values for your mocks
			chainNotifier.On("RegisterBlockEpochNtfn", mock.Anything).Return(
				blockChan, blockErrChan, nil,
			)
			go func() {
				// Send the block notification.
				if tc.blockHeight != 0 {
					blockChan <- tc.blockHeight
				}
			}()

			go func() {
				// Send the block notification error.
				if tc.blockErr != nil {
					blockErrChan <- tc.blockErr
				}
			}()

			spendChan := make(chan *chainntnfs.SpendDetail)
			spendErrChan := make(chan error)

			chainNotifier.On(
				"RegisterSpendNtfn", mock.Anything,
				mock.Anything, mock.Anything,
			).Return(
				spendChan, spendErrChan, nil,
			)

			go func() {
				// Send the spend notification.
				if tc.spendDetail != nil {
					spendChan <- tc.spendDetail
				}
			}()

			eventType := r.AsyncWaitForExpiredOrSweptAction(nil)
			// Assert that the return value is as expected
			require.Equal(t, fsm.NoOp, eventType)

			// Assert that the state is as expected
			r.DefaultObserver.WaitForState(
				r.ctx, time.Second*5, tc.expectedState,
			)

			// Assert that the expected functions were called on the mocks
			chainNotifier.AssertExpectations(t)
		})
	}
}
