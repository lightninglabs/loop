package utils

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
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

// stubConfirmationsRegistrar implements ConfirmationsRegistrar.
type stubConfirmationsRegistrar struct {
	mu           sync.Mutex
	attempts     int
	succeedAfter int
}

func (s *stubConfirmationsRegistrar) RegisterConfirmationsNtfn(
	context.Context, *chainhash.Hash, []byte, int32, int32,
	...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation, chan error,
	error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.attempts++
	if s.attempts <= s.succeedAfter {
		return nil, nil, status.Error(
			codes.Unknown, chainNotifierStartupMessage,
		)
	}

	return make(chan *chainntnfs.TxConfirmation), make(chan error), nil
}

func (s *stubConfirmationsRegistrar) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.attempts
}

// stubSpendRegistrar implements SpendRegistrar.
type stubSpendRegistrar struct {
	mu           sync.Mutex
	attempts     int
	succeedAfter int
}

func (s *stubSpendRegistrar) RegisterSpendNtfn(context.Context,
	*wire.OutPoint, []byte, int32, ...lndclient.NotifierOption) (
	chan *chainntnfs.SpendDetail, chan error, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.attempts++
	if s.attempts <= s.succeedAfter {
		return nil, nil, status.Error(
			codes.Unknown, chainNotifierStartupMessage,
		)
	}

	return make(chan *chainntnfs.SpendDetail), make(chan error), nil
}

func (s *stubSpendRegistrar) Attempts() int {
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

// TestRegisterConfirmationsNtfnWithRetry ensures confirmation subscriptions
// retry until the notifier becomes available.
func TestRegisterConfirmationsNtfnWithRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)

	stub := &stubConfirmationsRegistrar{
		succeedAfter: 2,
	}

	confChan, errChan, err := RegisterConfirmationsNtfnWithRetry(
		ctx, stub, nil, nil, 1, 0,
	)
	require.NoError(t, err)
	require.NotNil(t, confChan)
	require.NotNil(t, errChan)
	require.Equal(t, 3, stub.Attempts())
}

// TestRegisterConfirmationsNtfnWithRetryContextCancel ensures context
// cancellation is propagated while retrying confirmation subscriptions.
func TestRegisterConfirmationsNtfnWithRetryContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	t.Cleanup(cancel)

	stub := &stubConfirmationsRegistrar{
		succeedAfter: 100,
	}

	_, _, err := RegisterConfirmationsNtfnWithRetry(
		ctx, stub, nil, nil, 1, 0,
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, stub.Attempts(), 1)
}

// TestRegisterSpendNtfnWithRetry ensures spend subscriptions retry until the
// notifier becomes available.
func TestRegisterSpendNtfnWithRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)

	stub := &stubSpendRegistrar{
		succeedAfter: 1,
	}

	spendChan, errChan, err := RegisterSpendNtfnWithRetry(
		ctx, stub, nil, nil, 0,
	)
	require.NoError(t, err)
	require.NotNil(t, spendChan)
	require.NotNil(t, errChan)
	require.Equal(t, 2, stub.Attempts())
}

// TestRegisterSpendNtfnWithRetryContextCancel ensures spend subscription retry
// honours context cancellation.
func TestRegisterSpendNtfnWithRetryContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	t.Cleanup(cancel)

	stub := &stubSpendRegistrar{
		succeedAfter: 100,
	}

	_, _, err := RegisterSpendNtfnWithRetry(ctx, stub, nil, nil, 0)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, stub.Attempts(), 1)
}
