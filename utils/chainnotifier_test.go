package utils

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubBlockEpochRegistrar implements BlockEpochRegistrar and records the
// number of attempts before succeeding.
type stubBlockEpochRegistrar struct {
	mu           sync.Mutex
	attempts     int
	succeedAfter int
}

// RegisterBlockEpochNtfn simulates a chain notifier that returns a startup
// error until the configured number of attempts has been exhausted.
func (s *stubBlockEpochRegistrar) RegisterBlockEpochNtfn(
	context.Context) (chan int32, chan error, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.attempts++
	if s.attempts <= s.succeedAfter {
		return nil, nil, status.Error(
			codes.Unknown, chainNotifierStartupMessage,
		)
	}

	return make(chan int32), make(chan error), nil
}

// Attempts returns the total number of registration attempts made.
func (s *stubBlockEpochRegistrar) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.attempts
}

// TestRegisterBlockEpochNtfnWithRetry ensures we retry until the notifier
// becomes available.
func TestRegisterBlockEpochNtfnWithRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)

	stub := &stubBlockEpochRegistrar{
		succeedAfter: 1,
	}

	blockChan, errChan, err := RegisterBlockEpochNtfnWithRetry(ctx, stub)
	require.NoError(t, err)
	require.NotNil(t, blockChan)
	require.NotNil(t, errChan)
	require.Equal(t, 2, stub.Attempts())
}

// TestRegisterBlockEpochNtfnWithRetryContextCancel ensures we propagate the
// caller's context error if the notifier never becomes ready.
func TestRegisterBlockEpochNtfnWithRetryContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	t.Cleanup(cancel)

	stub := &stubBlockEpochRegistrar{
		succeedAfter: 100,
	}

	_, _, err := RegisterBlockEpochNtfnWithRetry(ctx, stub)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, stub.Attempts(), 1)
}
