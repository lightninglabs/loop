package tapsdk

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mockEventClient is a test double for EventClient that allows
// controlling event delivery and stream errors.
type mockEventClient struct {
	mu             sync.Mutex
	receiveSetup   func() (<-chan *ReceiveEventRecord, <-chan error)
	sendSetup      func() (<-chan *SendEventRecord, <-chan error)
	mintSetup      func() (<-chan *MintEvent, <-chan error)
	subscribeCalls int
}

func (m *mockEventClient) SubscribeReceiveEvents(_ context.Context,
	_ *SubscribeReceiveEventsRequest) (
	<-chan *ReceiveEventRecord, <-chan error, error) {

	m.mu.Lock()
	m.subscribeCalls++
	setup := m.receiveSetup
	m.mu.Unlock()

	if setup == nil {
		return nil, nil, io.EOF
	}

	evCh, errCh := setup()

	return evCh, errCh, nil
}

func (m *mockEventClient) SubscribeSendEvents(_ context.Context,
	_ *SubscribeSendEventsRequest) (
	<-chan *SendEventRecord, <-chan error, error) {

	m.mu.Lock()
	m.subscribeCalls++
	setup := m.sendSetup
	m.mu.Unlock()

	if setup == nil {
		return nil, nil, io.EOF
	}

	evCh, errCh := setup()

	return evCh, errCh, nil
}

func (m *mockEventClient) SubscribeMintEvents(_ context.Context,
	_ *SubscribeMintEventsRequest) (
	<-chan *MintEvent, <-chan error, error) {

	m.mu.Lock()
	m.subscribeCalls++
	setup := m.mintSetup
	m.mu.Unlock()

	if setup == nil {
		return nil, nil, io.EOF
	}

	evCh, errCh := setup()

	return evCh, errCh, nil
}

func (m *mockEventClient) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subscribeCalls
}

// TestEventListener_ReceiveEvents verifies that receive events are
// delivered to the handler.
func TestEventListener_ReceiveEvents(t *testing.T) {
	var received atomic.Int32

	mc := &mockEventClient{
		receiveSetup: func() (
			<-chan *ReceiveEventRecord, <-chan error) {

			evCh := make(chan *ReceiveEventRecord, 3)
			errCh := make(chan error, 1)

			evCh <- &ReceiveEventRecord{
				Timestamp: 1,
				Outpoint:  "txid:0",
			}
			evCh <- &ReceiveEventRecord{
				Timestamp: 2,
				Outpoint:  "txid:1",
			}

			// Close the channel after events.
			close(evCh)
			close(errCh)

			return evCh, errCh
		},
	}

	listener := NewEventListener(mc, EventHandler{
		OnReceive: func(_ context.Context,
			e *ReceiveEvent) {

			received.Add(1)
		},
	}, WithMaxRetries(1), WithInitialBackoff(10*time.Millisecond))

	ctx, cancel := context.WithTimeout(
		context.Background(), 2*time.Second,
	)
	defer cancel()

	err := listener.Start(ctx)
	require.NoError(t, err)

	// Wait for events to be processed.
	time.Sleep(200 * time.Millisecond)

	err = listener.Stop()
	require.NoError(t, err)

	require.GreaterOrEqual(t, received.Load(), int32(2))
}

// TestEventListener_Reconnect verifies that the listener reconnects
// after a retriable error.
func TestEventListener_Reconnect(t *testing.T) {
	var callCount atomic.Int32

	mc := &mockEventClient{
		receiveSetup: func() (
			<-chan *ReceiveEventRecord, <-chan error) {

			call := callCount.Add(1)

			evCh := make(chan *ReceiveEventRecord, 1)
			errCh := make(chan error, 1)

			if call == 1 {
				// First call: deliver one event then
				// fail with retriable error.
				evCh <- &ReceiveEventRecord{
					Timestamp: 1,
				}
				close(evCh)
				errCh <- io.EOF
			} else {
				// Second call: deliver event and close
				// cleanly.
				evCh <- &ReceiveEventRecord{
					Timestamp: 2,
				}
				close(evCh)
				close(errCh)
			}

			return evCh, errCh
		},
	}

	var received atomic.Int32
	listener := NewEventListener(mc, EventHandler{
		OnReceive: func(_ context.Context,
			_ *ReceiveEvent) {

			received.Add(1)
		},
	},
		WithMaxRetries(3),
		WithInitialBackoff(10*time.Millisecond),
		WithMaxBackoff(50*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(
		context.Background(), 3*time.Second,
	)
	defer cancel()

	err := listener.Start(ctx)
	require.NoError(t, err)

	// Poll until reconnection delivers the second event.
	require.Eventually(t, func() bool {
		return received.Load() >= 2
	}, 3*time.Second, 20*time.Millisecond,
		"expected at least 2 events via reconnect",
	)

	require.NoError(t, listener.Stop())

	require.GreaterOrEqual(t, callCount.Load(), int32(2))
}

// TestEventListener_OnDisconnect verifies that transient stream
// breaks fire OnDisconnect with the terminal error and planned
// retry delay, without involving OnError.
func TestEventListener_OnDisconnect(t *testing.T) {
	var callCount atomic.Int32

	mc := &mockEventClient{
		receiveSetup: func() (
			<-chan *ReceiveEventRecord, <-chan error) {

			call := callCount.Add(1)

			evCh := make(chan *ReceiveEventRecord, 1)
			errCh := make(chan error, 1)

			if call == 1 {
				// First call: break with a retriable error.
				close(evCh)
				errCh <- io.EOF
			} else {
				// Second call: deliver event and close
				// cleanly so the test terminates.
				evCh <- &ReceiveEventRecord{Timestamp: 1}
				close(evCh)
				close(errCh)
			}

			return evCh, errCh
		},
	}

	type disconnect struct {
		stream    string
		err       error
		nextRetry time.Duration
	}

	var (
		disconnects   []disconnect
		disconnectsMu sync.Mutex
		gotFatal      atomic.Bool
	)

	listener := NewEventListener(mc, EventHandler{
		OnReceive: func(_ context.Context,
			_ *ReceiveEvent) {
		},
		OnDisconnect: func(stream string, err error,
			nextRetry time.Duration) {

			disconnectsMu.Lock()
			disconnects = append(disconnects, disconnect{
				stream:    stream,
				err:       err,
				nextRetry: nextRetry,
			})
			disconnectsMu.Unlock()
		},
		OnError: func(_ string, _ error) {
			gotFatal.Store(true)
		},
	},
		WithMaxRetries(3),
		WithInitialBackoff(10*time.Millisecond),
		WithMaxBackoff(50*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(
		context.Background(), 2*time.Second,
	)
	defer cancel()

	require.NoError(t, listener.Start(ctx))

	// Wait until we've observed at least one disconnect callback.
	require.Eventually(t, func() bool {
		disconnectsMu.Lock()
		defer disconnectsMu.Unlock()
		return len(disconnects) >= 1
	}, 2*time.Second, 10*time.Millisecond,
		"expected OnDisconnect to fire on retriable break",
	)

	require.NoError(t, listener.Stop())

	disconnectsMu.Lock()
	defer disconnectsMu.Unlock()

	require.Equal(t, "receive", disconnects[0].stream)
	require.ErrorIs(t, disconnects[0].err, io.EOF)
	require.Greater(t, disconnects[0].nextRetry, time.Duration(0))

	// Retriable break must not trigger the terminal OnError hook.
	require.False(t, gotFatal.Load())
}

// TestEventListener_FatalError verifies that fatal errors stop the
// stream without retrying.
func TestEventListener_FatalError(t *testing.T) {
	mc := &mockEventClient{
		receiveSetup: func() (
			<-chan *ReceiveEventRecord, <-chan error) {

			evCh := make(chan *ReceiveEventRecord)
			errCh := make(chan error, 1)

			// Send a fatal error immediately.
			close(evCh)
			errCh <- status.Error(
				codes.PermissionDenied,
				"bad macaroon",
			)

			return evCh, errCh
		},
	}

	var errorStream string
	var errorErr error
	var errorMu sync.Mutex

	listener := NewEventListener(mc, EventHandler{
		OnReceive: func(_ context.Context,
			_ *ReceiveEvent) {

			t.Fatal("should not receive events")
		},
		OnError: func(stream string, err error) {
			errorMu.Lock()
			errorStream = stream
			errorErr = err
			errorMu.Unlock()
		},
	}, WithMaxRetries(5), WithInitialBackoff(10*time.Millisecond))

	ctx, cancel := context.WithTimeout(
		context.Background(), 2*time.Second,
	)
	defer cancel()

	err := listener.Start(ctx)
	require.NoError(t, err)

	time.Sleep(200 * time.Millisecond)
	require.NoError(t, listener.Stop())

	errorMu.Lock()
	defer errorMu.Unlock()

	require.Equal(t, "receive", errorStream)
	require.Error(t, errorErr)

	s, ok := status.FromError(errorErr)
	require.True(t, ok)
	require.Equal(t, codes.PermissionDenied, s.Code())

	// Should have only called subscribe once (no retry).
	require.Equal(t, 1, mc.calls())
}

// TestEventListener_MaxRetries verifies that the listener gives up
// after MaxRetries.
func TestEventListener_MaxRetries(t *testing.T) {
	mc := &mockEventClient{
		receiveSetup: func() (
			<-chan *ReceiveEventRecord, <-chan error) {

			evCh := make(chan *ReceiveEventRecord)
			errCh := make(chan error, 1)

			close(evCh)
			errCh <- io.EOF

			return evCh, errCh
		},
	}

	var gotError atomic.Bool

	listener := NewEventListener(mc, EventHandler{
		OnReceive: func(_ context.Context,
			_ *ReceiveEvent) {
		},
		OnError: func(_ string, _ error) {
			gotError.Store(true)
		},
	},
		WithMaxRetries(3),
		WithInitialBackoff(5*time.Millisecond),
		WithMaxBackoff(10*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(
		context.Background(), 3*time.Second,
	)
	defer cancel()

	err := listener.Start(ctx)
	require.NoError(t, err)

	time.Sleep(500 * time.Millisecond)
	require.NoError(t, listener.Stop())

	require.True(t, gotError.Load())
	// 1 initial + 3 retries = 4 calls.
	require.Equal(t, 4, mc.calls())
}

// TestEventListener_Stop verifies graceful shutdown.
func TestEventListener_Stop(t *testing.T) {
	// Create a stream that blocks forever.
	mc := &mockEventClient{
		receiveSetup: func() (
			<-chan *ReceiveEventRecord, <-chan error) {

			return make(chan *ReceiveEventRecord),
				make(chan error, 1)
		},
	}

	listener := NewEventListener(mc, EventHandler{
		OnReceive: func(_ context.Context,
			_ *ReceiveEvent) {
		},
	})

	ctx := context.Background()
	err := listener.Start(ctx)
	require.NoError(t, err)
	require.True(t, listener.Running())

	// Stop should not hang.
	done := make(chan struct{})
	go func() {
		_ = listener.Stop()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() timed out")
	}

	require.False(t, listener.Running())
}

// TestEventListener_NilHandlers verifies that nil handlers don't
// start goroutines.
func TestEventListener_NilHandlers(t *testing.T) {
	mc := &mockEventClient{}

	listener := NewEventListener(mc, EventHandler{})

	err := listener.Start(context.Background())
	require.NoError(t, err)

	time.Sleep(50 * time.Millisecond)
	require.NoError(t, listener.Stop())

	// No subscribe calls should have been made.
	require.Equal(t, 0, mc.calls())
}

// TestEventListener_MultipleStreams verifies that all three stream
// types work concurrently.
func TestEventListener_MultipleStreams(t *testing.T) {
	var receiveCount, sendCount, mintCount atomic.Int32

	mc := &mockEventClient{
		receiveSetup: func() (
			<-chan *ReceiveEventRecord, <-chan error) {

			ch := make(chan *ReceiveEventRecord, 1)
			ch <- &ReceiveEventRecord{Timestamp: 1}
			close(ch)

			return ch, make(chan error)
		},
		sendSetup: func() (
			<-chan *SendEventRecord, <-chan error) {

			ch := make(chan *SendEventRecord, 1)
			ch <- &SendEventRecord{Timestamp: 1}
			close(ch)

			return ch, make(chan error)
		},
		mintSetup: func() (
			<-chan *MintEvent, <-chan error) {

			ch := make(chan *MintEvent, 1)
			ch <- &MintEvent{Timestamp: 1}
			close(ch)

			return ch, make(chan error)
		},
	}

	listener := NewEventListener(mc, EventHandler{
		OnReceive: func(_ context.Context,
			_ *ReceiveEvent) {

			receiveCount.Add(1)
		},
		OnSend: func(_ context.Context,
			_ *SendEvent) {

			sendCount.Add(1)
		},
		OnMint: func(_ context.Context,
			_ *MintEvent) {

			mintCount.Add(1)
		},
	}, WithMaxRetries(1), WithInitialBackoff(10*time.Millisecond))

	ctx, cancel := context.WithTimeout(
		context.Background(), 2*time.Second,
	)
	defer cancel()

	err := listener.Start(ctx)
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)
	require.NoError(t, listener.Stop())

	require.GreaterOrEqual(t, receiveCount.Load(), int32(1))
	require.GreaterOrEqual(t, sendCount.Load(), int32(1))
	require.GreaterOrEqual(t, mintCount.Load(), int32(1))
}

// TestAddJitter verifies jitter stays within bounds.
func TestAddJitter(t *testing.T) {
	base := 100 * time.Millisecond
	minExpected := time.Duration(
		float64(base) * (1 - jitterFraction),
	)
	maxExpected := time.Duration(
		float64(base) * (1 + jitterFraction),
	)

	for range 100 {
		jittered := addJitter(base)
		require.GreaterOrEqual(t, jittered, minExpected)
		require.LessOrEqual(t, jittered, maxExpected)
	}
}

// TestIsFatalError verifies error classification.
func TestIsFatalError(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		fatal bool
	}{
		{
			name:  "nil error",
			err:   nil,
			fatal: false,
		},
		{
			name:  "io.EOF",
			err:   io.EOF,
			fatal: false,
		},
		{
			name: "PermissionDenied",
			err: status.Error(
				codes.PermissionDenied, "denied",
			),
			fatal: true,
		},
		{
			name: "Unauthenticated",
			err: status.Error(
				codes.Unauthenticated, "no mac",
			),
			fatal: true,
		},
		{
			name: "Unimplemented",
			err: status.Error(
				codes.Unimplemented, "no rpc",
			),
			fatal: true,
		},
		{
			name: "Unavailable",
			err: status.Error(
				codes.Unavailable, "server down",
			),
			fatal: false,
		},
		{
			name: "DeadlineExceeded",
			err: status.Error(
				codes.DeadlineExceeded, "timeout",
			),
			fatal: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.fatal, isFatalError(tt.err))
		})
	}
}
