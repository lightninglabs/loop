package notifications

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

var (
	testReservationId  = []byte{0x01, 0x02}
	testReservationId2 = []byte{0x01, 0x02}
)

// mockNotificationsClient implements the NotificationsClient interface for testing.
type mockNotificationsClient struct {
	mockStream   swapserverrpc.SwapServer_SubscribeNotificationsClient
	subscribeErr error
	timesCalled  int
	sync.Mutex
}

func (m *mockNotificationsClient) SubscribeNotifications(ctx context.Context,
	in *swapserverrpc.SubscribeNotificationsRequest,
	opts ...grpc.CallOption) (
	swapserverrpc.SwapServer_SubscribeNotificationsClient, error) {

	m.Lock()
	defer m.Unlock()

	m.timesCalled++
	if m.subscribeErr != nil {
		return nil, m.subscribeErr
	}
	return m.mockStream, nil
}

// mockSubscribeNotificationsClient simulates the server stream.
type mockSubscribeNotificationsClient struct {
	grpc.ClientStream
	recvChan    chan *swapserverrpc.SubscribeNotificationsResponse
	recvErrChan chan error
}

func (m *mockSubscribeNotificationsClient) Recv() (
	*swapserverrpc.SubscribeNotificationsResponse, error) {

	select {
	case err := <-m.recvErrChan:
		return nil, err
	case notif, ok := <-m.recvChan:
		if !ok {
			return nil, io.EOF
		}
		return notif, nil
	}
}

func (m *mockSubscribeNotificationsClient) Header() (metadata.MD, error) {
	return nil, nil
}

func (m *mockSubscribeNotificationsClient) Trailer() metadata.MD {
	return nil
}

func (m *mockSubscribeNotificationsClient) CloseSend() error {
	return nil
}

func (m *mockSubscribeNotificationsClient) Context() context.Context {
	return context.TODO()
}

func (m *mockSubscribeNotificationsClient) SendMsg(interface{}) error {
	return nil
}

func (m *mockSubscribeNotificationsClient) RecvMsg(interface{}) error {
	return nil
}

func TestManager_ReservationNotification(t *testing.T) {
	// Create a mock notification client
	recvChan := make(chan *swapserverrpc.SubscribeNotificationsResponse, 1)
	errChan := make(chan error, 1)
	mockStream := &mockSubscribeNotificationsClient{
		recvChan:    recvChan,
		recvErrChan: errChan,
	}
	mockClient := &mockNotificationsClient{
		mockStream: mockStream,
	}

	// Create a Manager with the mock client
	mgr := NewManager(&Config{
		Client: mockClient,
		CurrentToken: func() (*l402.Token, error) {
			// Simulate successful fetching of L402
			return nil, nil
		},
	})

	// Subscribe to reservation notifications.
	subCtx, subCancel := context.WithCancel(context.Background())
	subChan := mgr.SubscribeReservations(subCtx)

	// Run the manager.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		err := mgr.Run(ctx)
		require.NoError(t, err)
	}()

	// Wait a bit to ensure manager is running and has subscribed
	require.Eventually(t, func() bool {
		mgr.Lock()
		defer mgr.Unlock()
		return len(mgr.subscribers[NotificationTypeReservation]) > 0
	}, time.Second*5, 10*time.Millisecond)

	mockClient.Lock()
	require.Equal(t, 1, mockClient.timesCalled)
	mockClient.Unlock()

	// Send a test notification
	testNotif := getTestNotification(testReservationId)

	// Send the notification to the recvChan
	recvChan <- testNotif

	// Collect the notification in the callback
	receivedNotification := <-subChan

	// Now, check that the notification received in the callback matches the one sent
	require.NotNil(t, receivedNotification)
	require.Equal(t, testReservationId, receivedNotification.ReservationId)

	// Cancel the subscription
	subCancel()

	// Send another test notification`
	testNotif2 := getTestNotification(testReservationId2)
	recvChan <- testNotif2

	// Check that the subChan is eventually closed.
	require.Eventually(t, func() bool {
		select {
		case _, ok := <-subChan:
			return !ok
		default:
			return false
		}
	}, time.Second*5, 10*time.Millisecond)
}

func getTestNotification(resId []byte) *swapserverrpc.SubscribeNotificationsResponse {
	return &swapserverrpc.SubscribeNotificationsResponse{
		Notification: &swapserverrpc.SubscribeNotificationsResponse_ReservationNotification{
			ReservationNotification: &swapserverrpc.ServerReservationNotification{
				ReservationId: resId,
			},
		},
	}
}
