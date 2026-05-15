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
	testReservationId2 = []byte{0x03, 0x04}
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

func (m *mockSubscribeNotificationsClient) SendMsg(any) error {
	return nil
}

func (m *mockSubscribeNotificationsClient) RecvMsg(any) error {
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
	ctx := t.Context()

	go func() {
		err := mgr.Run(ctx)
		require.NoError(t, err)
	}()

	// Wait a bit to ensure manager is running and has subscribed to the
	// server notifications stream.
	require.Eventually(t, func() bool {
		mockClient.Lock()
		defer mockClient.Unlock()

		return mockClient.timesCalled > 0
	}, time.Second*5, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		mockClient.Lock()
		defer mockClient.Unlock()
		return mockClient.timesCalled == 1
	}, time.Second*5, 10*time.Millisecond)

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

// unfinishedSwapNotification builds an unfinished swap notification.
func unfinishedSwapNotification(
	swapHash lntypes.Hash) *swapserverrpc.SubscribeNotificationsResponse {

	return &swapserverrpc.SubscribeNotificationsResponse{
		Notification: &swapserverrpc.
			SubscribeNotificationsResponse_UnfinishedSwap{
			UnfinishedSwap: &swapserverrpc.
				ServerUnfinishedSwapNotification{
				SwapHash: swapHash[:],
			},
		},
	}
}

func staticLoopInRiskAcceptedNotification(
	swapHash lntypes.Hash) *swapserverrpc.SubscribeNotificationsResponse {

	return &swapserverrpc.SubscribeNotificationsResponse{
		Notification: &swapserverrpc.
			SubscribeNotificationsResponse_StaticLoopInRiskAccepted{
			StaticLoopInRiskAccepted: &swapserverrpc.
				ServerStaticLoopInRiskAcceptedNotification{
				SwapHash: swapHash[:],
			},
		},
	}
}

// staticLoopInRiskRejectedNotification builds a risk rejected notification.
func staticLoopInRiskRejectedNotification(
	swapHash lntypes.Hash) *swapserverrpc.SubscribeNotificationsResponse {

	return &swapserverrpc.SubscribeNotificationsResponse{
		Notification: &swapserverrpc.
			SubscribeNotificationsResponse_StaticLoopInRiskRejected{
			StaticLoopInRiskRejected: &swapserverrpc.
				ServerStaticLoopInRiskRejectedNotification{
				SwapHash: swapHash[:],
			},
		},
	}
}

type staticLoopInRiskNotification interface {
	GetSwapHash() []byte
}

// assertStaticLoopInRiskNotificationSwapScoped checks swap-scoped fanout.
func assertStaticLoopInRiskNotificationSwapScoped[
	T staticLoopInRiskNotification](t *testing.T,
	subscribe func(*Manager, context.Context, lntypes.Hash) <-chan T,
	notification func(lntypes.Hash) *swapserverrpc.
		SubscribeNotificationsResponse, label string,
	swapHashA, swapHashB lntypes.Hash) {

	t.Helper()

	mgr := NewManager(&Config{})

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()

	subChanA := subscribe(mgr, subCtx, swapHashA)
	subChanB := subscribe(mgr, subCtx, swapHashB)

	mgr.handleNotification(t.Context(), notification(swapHashA))

	select {
	case received := <-subChanA:
		require.Equal(t, swapHashA[:], received.GetSwapHash())

	case <-time.After(time.Second):
		t.Fatalf("did not receive first swap risk %s notification",
			label)
	}

	select {
	case received := <-subChanB:
		t.Fatalf("second swap received wrong notification: %x",
			received.GetSwapHash())

	default:
	}

	mgr.handleNotification(t.Context(), notification(swapHashB))

	select {
	case received := <-subChanB:
		require.Equal(t, swapHashB[:], received.GetSwapHash())

	case <-time.After(time.Second):
		t.Fatalf("did not receive second swap risk %s notification",
			label)
	}
}

// TestManager_SlowSubscriberDoesNotBlock tests that a subscriber with a full
// notification channel does not block delivery to other subscribers.
func TestManager_SlowSubscriberDoesNotBlock(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&Config{})

	slowCtx, slowCancel := context.WithCancel(t.Context())
	defer slowCancel()
	slowChan := mgr.SubscribeReservations(slowCtx)

	fastCtx, fastCancel := context.WithCancel(t.Context())
	defer fastCancel()
	fastChan := mgr.SubscribeReservations(fastCtx)

	firstNotif := getTestNotification(testReservationId)
	mgr.handleNotification(t.Context(), firstNotif)

	received := <-fastChan
	require.Equal(t, testReservationId, received.ReservationId)

	secondNotif := getTestNotification(testReservationId2)
	done := make(chan struct{})
	go func() {
		mgr.handleNotification(t.Context(), secondNotif)
		close(done)
	}()

	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	select {
	case received = <-fastChan:
		require.Equal(t, testReservationId2, received.ReservationId)

	case <-time.After(time.Second):
		t.Fatal("fast subscriber did not receive notification")
	}

	require.Len(t, slowChan, 1)
}

// TestManager_UnfinishedSwapNotificationWaitsForSubscriber verifies that
// unfinished swap recovery notifications are not dropped when the local
// subscriber is briefly behind.
func TestManager_UnfinishedSwapNotificationWaitsForSubscriber(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&Config{})

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()

	subChan := mgr.SubscribeUnfinishedSwaps(subCtx)

	swapHashA := lntypes.Hash{0x02, 0x03}
	swapHashB := lntypes.Hash{0x04, 0x05}

	mgr.handleNotification(t.Context(), unfinishedSwapNotification(swapHashA))

	done := make(chan struct{})
	go func() {
		mgr.handleNotification(t.Context(), unfinishedSwapNotification(swapHashB))
		close(done)
	}()

	select {
	case received := <-subChan:
		require.Equal(t, swapHashA[:], received.SwapHash)

	case <-time.After(time.Second):
		t.Fatal("did not receive first unfinished swap notification")
	}

	select {
	case <-done:

	case <-time.After(time.Second):
		t.Fatal("second unfinished swap notification did not unblock")
	}

	select {
	case received := <-subChan:
		require.Equal(t, swapHashB[:], received.SwapHash)

	case <-time.After(time.Second):
		t.Fatal("second unfinished swap notification was dropped")
	}
}

// TestManager_StaticLoopInRiskAcceptedNotification tests that the Manager
// forwards static loop in risk accepted notifications to subscribers.
func TestManager_StaticLoopInRiskAcceptedNotification(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&Config{})

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()

	swapHash := lntypes.Hash{0x04, 0x05}

	subChan := mgr.SubscribeStaticLoopInRiskAccepted(subCtx, swapHash)

	mgr.handleNotification(
		t.Context(),
		&swapserverrpc.SubscribeNotificationsResponse{
			Notification: &swapserverrpc.
				SubscribeNotificationsResponse_StaticLoopInRiskAccepted{
				StaticLoopInRiskAccepted: &swapserverrpc.
					ServerStaticLoopInRiskAcceptedNotification{
					SwapHash: swapHash[:],
				},
			},
		},
	)

	select {
	case received := <-subChan:
		require.Equal(t, swapHash[:], received.SwapHash)

	case <-time.After(time.Second):
		t.Fatal("did not receive risk accepted notification")
	}
}

// TestManager_StaticLoopInRiskDecisionPersists verifies that risk decisions are
// handed to the durable callback before they are treated as delivered.
func TestManager_StaticLoopInRiskDecisionPersists(t *testing.T) {
	t.Parallel()

	type persistedDecision struct {
		swapHash lntypes.Hash
		accepted bool
	}

	persisted := make(chan persistedDecision, 2)
	mgr := NewManager(&Config{
		PersistStaticLoopInRiskDecision: func(_ context.Context,
			swapHash lntypes.Hash, accepted bool) error {

			persisted <- persistedDecision{
				swapHash: swapHash,
				accepted: accepted,
			}

			return nil
		},
	})

	acceptedHash := lntypes.Hash{0x16, 0x17}
	rejectedHash := lntypes.Hash{0x18, 0x19}

	mgr.handleNotification(
		t.Context(), staticLoopInRiskAcceptedNotification(acceptedHash),
	)
	mgr.handleNotification(
		t.Context(), staticLoopInRiskRejectedNotification(rejectedHash),
	)

	select {
	case decision := <-persisted:
		require.Equal(t, acceptedHash, decision.swapHash)
		require.True(t, decision.accepted)

	case <-time.After(time.Second):
		t.Fatal("accepted risk decision was not persisted")
	}

	select {
	case decision := <-persisted:
		require.Equal(t, rejectedHash, decision.swapHash)
		require.False(t, decision.accepted)

	case <-time.After(time.Second):
		t.Fatal("rejected risk decision was not persisted")
	}
}

// TestManager_StaticLoopInRiskDecisionReplayOnPersistFailure verifies that an
// early risk notification is still cached if the swap row does not exist yet.
func TestManager_StaticLoopInRiskDecisionReplayOnPersistFailure(t *testing.T) {
	t.Parallel()

	swapHash := lntypes.Hash{0x1a, 0x1b}
	mgr := NewManager(&Config{
		PersistStaticLoopInRiskDecision: func(_ context.Context,
			_ lntypes.Hash, _ bool) error {

			return errors.New("swap not stored yet")
		},
	})

	mgr.handleNotification(
		t.Context(), staticLoopInRiskAcceptedNotification(swapHash),
	)

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()

	subChan := mgr.SubscribeStaticLoopInRiskAccepted(subCtx, swapHash)

	select {
	case received := <-subChan:
		require.Equal(t, swapHash[:], received.SwapHash)

	case <-time.After(time.Second):
		t.Fatal("did not replay risk notification after persist failure")
	}
}

// TestManager_StaticLoopInRiskAcceptedNotificationSwapScoped verifies that a
// notification for one swap does not occupy another swap's subscriber channel.
func TestManager_StaticLoopInRiskAcceptedNotificationSwapScoped(t *testing.T) {
	t.Parallel()

	assertStaticLoopInRiskNotificationSwapScoped(
		t, func(m *Manager, ctx context.Context,
			swapHash lntypes.Hash) <-chan *swapserverrpc.
			ServerStaticLoopInRiskAcceptedNotification {

			return m.SubscribeStaticLoopInRiskAccepted(ctx, swapHash)
		}, staticLoopInRiskAcceptedNotification, "accepted",
		lntypes.Hash{0x04, 0x05}, lntypes.Hash{0x06, 0x07},
	)
}

// TestManager_StaticLoopInRiskAcceptedNotificationReplay tests that the Manager
// replays a risk accepted notification that arrives before the swap-specific
// subscriber is registered.
func TestManager_StaticLoopInRiskAcceptedNotificationReplay(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&Config{})

	swapHash := lntypes.Hash{0x06, 0x07}
	mgr.handleNotification(
		t.Context(),
		&swapserverrpc.SubscribeNotificationsResponse{
			Notification: &swapserverrpc.
				SubscribeNotificationsResponse_StaticLoopInRiskAccepted{
				StaticLoopInRiskAccepted: &swapserverrpc.
					ServerStaticLoopInRiskAcceptedNotification{
					SwapHash: swapHash[:],
				},
			},
		},
	)

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()

	subChan := mgr.SubscribeStaticLoopInRiskAccepted(subCtx, swapHash)

	select {
	case received := <-subChan:
		require.Equal(t, swapHash[:], received.SwapHash)

	case <-time.After(time.Second):
		t.Fatal("did not replay risk accepted notification")
	}
}

// TestManager_StaticLoopInRiskRejectedNotification tests that the Manager
// forwards static loop in risk rejected notifications to subscribers.
func TestManager_StaticLoopInRiskRejectedNotification(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&Config{})

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()

	swapHash := lntypes.Hash{0x08, 0x09}

	subChan := mgr.SubscribeStaticLoopInRiskRejected(subCtx, swapHash)

	mgr.handleNotification(
		t.Context(),
		&swapserverrpc.SubscribeNotificationsResponse{
			Notification: &swapserverrpc.
				SubscribeNotificationsResponse_StaticLoopInRiskRejected{
				StaticLoopInRiskRejected: &swapserverrpc.
					ServerStaticLoopInRiskRejectedNotification{
					SwapHash: swapHash[:],
				},
			},
		},
	)

	select {
	case received := <-subChan:
		require.Equal(t, swapHash[:], received.SwapHash)

	case <-time.After(time.Second):
		t.Fatal("did not receive risk rejected notification")
	}
}

// TestManager_StaticLoopInRiskRejectedNotificationSwapScoped verifies that a
// notification for one swap does not occupy another swap's subscriber channel.
func TestManager_StaticLoopInRiskRejectedNotificationSwapScoped(t *testing.T) {
	t.Parallel()

	assertStaticLoopInRiskNotificationSwapScoped(
		t, func(m *Manager, ctx context.Context,
			swapHash lntypes.Hash) <-chan *swapserverrpc.
			ServerStaticLoopInRiskRejectedNotification {

			return m.SubscribeStaticLoopInRiskRejected(ctx, swapHash)
		}, staticLoopInRiskRejectedNotification, "rejected",
		lntypes.Hash{0x08, 0x09}, lntypes.Hash{0x0a, 0x0b},
	)
}

// TestManager_StaticLoopInRiskRejectedNotificationReplay tests that the Manager
// replays a risk rejected notification that arrives before the swap-specific
// subscriber is registered.
func TestManager_StaticLoopInRiskRejectedNotificationReplay(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&Config{})

	swapHash := lntypes.Hash{0x0a, 0x0b}
	mgr.handleNotification(
		t.Context(),
		&swapserverrpc.SubscribeNotificationsResponse{
			Notification: &swapserverrpc.
				SubscribeNotificationsResponse_StaticLoopInRiskRejected{
				StaticLoopInRiskRejected: &swapserverrpc.
					ServerStaticLoopInRiskRejectedNotification{
					SwapHash: swapHash[:],
				},
			},
		},
	)

	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()

	subChan := mgr.SubscribeStaticLoopInRiskRejected(subCtx, swapHash)

	select {
	case received := <-subChan:
		require.Equal(t, swapHash[:], received.SwapHash)

	case <-time.After(time.Second):
		t.Fatal("did not replay risk rejected notification")
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
	wg.Go(func() {
		// We ignore the returned error because the Manager returns
		// nil on context cancel.
		_ = mgr.Run(ctx)
	})

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
	wg.Go(func() {
		_ = mgr.Run(ctx)
	})

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
	wg.Go(func() {
		// We ignore the returned error because the Manager returns
		// nil on context cancel.
		_ = mgr.Run(ctx)
	})

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
