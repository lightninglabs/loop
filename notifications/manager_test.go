package notifications

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lntypes"
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
	sync.Mutex

	mockStream   swapserverrpc.SwapServer_SubscribeNotificationsClient
	subscribeErr error
	attemptTimes []time.Time
	timesCalled  int
}

func (m *mockNotificationsClient) SubscribeNotifications(ctx context.Context,
	in *swapserverrpc.SubscribeNotificationsRequest,
	opts ...grpc.CallOption) (
	swapserverrpc.SwapServer_SubscribeNotificationsClient, error) {

	m.Lock()
	defer m.Unlock()

	m.timesCalled++
	m.attemptTimes = append(m.attemptTimes, time.Now())
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

// TestManager_ReservationNotification tests that the Manager correctly
// forwards reservation notifications to subscribers.
func TestManager_ReservationNotification(t *testing.T) {
	t.Parallel()

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
			return &l402.Token{
				Preimage: lntypes.Preimage{1, 2, 3},
			}, nil
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

// TestManager_Backoff verifies that repeated failures in
// subscribeNotifications cause the Manager to space out subscription attempts
// via a predictable incremental backoff.
func TestManager_Backoff(t *testing.T) {
	t.Parallel()

	// We'll tolerate a bit of jitter in the timing checks.
	const tolerance = 300 * time.Millisecond

	recvChan := make(chan *swapserverrpc.SubscribeNotificationsResponse)
	recvErrChan := make(chan error)

	mockStream := &mockSubscribeNotificationsClient{
		recvChan:    recvChan,
		recvErrChan: recvErrChan,
	}

	// Create a new mock client that will fail to subscribe.
	mockClient := &mockNotificationsClient{
		mockStream:   mockStream,
		subscribeErr: errors.New("failing on purpose"),
	}

	// Manager with a successful CurrentToken so that it always tries
	// to subscribe.
	mgr := NewManager(&Config{
		Client: mockClient,
		CurrentToken: func() (*l402.Token, error) {
			// Simulate successful fetching of L402
			return &l402.Token{
				Preimage: lntypes.Preimage{1, 2, 3},
			}, nil
		},
	})

	// Run the manager in a background goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// We ignore the returned error because the Manager returns
		// nil on context cancel.
		_ = mgr.Run(ctx)
	}()

	// Wait long enough to see at least 3 subscription attempts using
	// the Manager's default pattern.
	// We'll wait ~5 seconds total so we capture at least 3 attempts:
	//   - Attempt #1: immediate
	//   - Attempt #2: ~1 second
	//   - Attempt #3: ~3 seconds after that etc.
	time.Sleep(5 * time.Second)

	// Cancel the context to stop the manager.
	cancel()
	wg.Wait()

	// Check how many attempts we made.
	require.GreaterOrEqual(t, len(mockClient.attemptTimes), 3,
		"expected at least 3 attempts within 5 seconds",
	)

	expectedDelay := time.Second
	for i := 1; i < len(mockClient.attemptTimes); i++ {
		// The expected delay for the i-th gap (comparing attempt i to
		// attempt i-1) is i seconds (because the manager increments
		// the backoff by 1 second each time).
		actualDelay := mockClient.attemptTimes[i].Sub(
			mockClient.attemptTimes[i-1],
		)

		require.InDeltaf(
			t, expectedDelay, actualDelay, float64(tolerance),
			"Attempt %d -> Attempt %d delay should be ~%v, got %v",
			i, i+1, expectedDelay, actualDelay,
		)

		expectedDelay += time.Second
	}
}

// TestManager_MinAliveConnTime verifies that the Manager enforces the minimum
// alive connection time before considering a subscription successful.
func TestManager_MinAliveConnTime(t *testing.T) {
	t.Parallel()

	// Tolerance to allow for scheduling jitter.
	const tolerance = 300 * time.Millisecond

	// Set a small MinAliveConnTime so the test doesn't run too long.
	// Once a subscription stays alive longer than 2s, the manager resets
	// its backoff to 10s on the next loop iteration.
	const minAlive = 1 * time.Second

	// We'll provide a channel for incoming notifications
	// and another for forcing errors to close the subscription.
	recvChan := make(chan *swapserverrpc.SubscribeNotificationsResponse)
	recvErrChan := make(chan error)

	mockStream := &mockSubscribeNotificationsClient{
		recvChan:    recvChan,
		recvErrChan: recvErrChan,
	}

	// No immediate error from SubscribeNotifications, so it "succeeds".
	// We trigger subscription closure by sending an error to recvErrChan.
	mockClient := &mockNotificationsClient{
		mockStream: mockStream,
		// subscribeErr stays nil => success on each call.
	}

	// Create a Manager that uses our mock client and enforces
	// MinAliveConnTime=2s.
	mgr := NewManager(&Config{
		Client:           mockClient,
		MinAliveConnTime: minAlive,
		CurrentToken: func() (*l402.Token, error) {
			// Simulate successful fetching of L402
			return &l402.Token{
				Preimage: lntypes.Preimage{1, 2, 3},
			}, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = mgr.Run(ctx)
	}()

	// Let the subscription stay alive for 2s, which is >1s (minAlive).
	// Then force an error to end the subscription. The manager sees
	// it stayed connected ~2s and resets its backoff to 10s.
	go func() {
		time.Sleep(2 * time.Second)
		recvErrChan <- errors.New("mock subscription closed")
	}()

	// Wait enough time (~13s) to see:
	//  - First subscription (2s)
	//  - Manager resets to 10s
	//  - Second subscription attempt starts ~10s later.
	time.Sleep(13 * time.Second)

	// Signal EOF so the subscription stops.
	close(recvChan)

	// Stop the manager and wait for cleanup.
	cancel()
	wg.Wait()

	// Expect at least 2 attempts in attemptTimes:
	//  1) The one that stayed alive for 2s,
	//  2) The next attempt ~10s after that.
	require.GreaterOrEqual(
		t, len(mockClient.attemptTimes), 2,
		"expected at least 2 attempts with a successful subscription",
	)

	require.InDeltaf(
		t, 12*time.Second,
		mockClient.attemptTimes[1].Sub(mockClient.attemptTimes[0]),
		float64(tolerance),
		"Second attempt should occur ~2s after the first",
	)
}

// TestManager_Backoff_Pending_Token verifies that the Manager backs off when
// the token is pending.
func TestManager_Backoff_Pending_Token(t *testing.T) {
	t.Parallel()

	// We'll tolerate a bit of jitter in the timing checks.
	const tolerance = 300 * time.Millisecond

	recvChan := make(chan *swapserverrpc.SubscribeNotificationsResponse)
	recvErrChan := make(chan error)

	mockStream := &mockSubscribeNotificationsClient{
		recvChan:    recvChan,
		recvErrChan: recvErrChan,
	}

	// Create a new mock client that will fail to subscribe.
	mockClient := &mockNotificationsClient{
		mockStream: mockStream,
		// subscribeErr stays nil => would succeed on each call.
	}

	var tokenCalls []time.Time
	// Manager with a successful CurrentToken so that it always tries
	// to subscribe.
	mgr := NewManager(&Config{
		Client: mockClient,
		CurrentToken: func() (*l402.Token, error) {
			tokenCalls = append(tokenCalls, time.Now())
			if len(tokenCalls) < 3 {
				// Simulate a pending token.
				return &l402.Token{}, nil
			}

			// Simulate successful fetching of L402
			return &l402.Token{
				Preimage: lntypes.Preimage{1, 2, 3},
			}, nil
		},
	})

	// Run the manager in a background goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// We ignore the returned error because the Manager returns
		// nil on context cancel.
		_ = mgr.Run(ctx)
	}()

	// Wait long enough to see at least 3 token calls, so we can see that
	// we'll indeed backoff when the token is pending.
	time.Sleep(5 * time.Second)

	// Signal EOF so the subscription stops.
	close(recvChan)

	// Cancel the context to stop the manager.
	cancel()
	wg.Wait()

	// Expect exactly 3 token calls.
	require.Len(t, tokenCalls, 3)

	require.InDeltaf(
		t, 3*time.Second, tokenCalls[2].Sub(tokenCalls[0]),
		float64(tolerance),
		"Expected to backoff for at ~3 seconds due to pending token",
	)
}
