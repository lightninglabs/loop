package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type readinessBackend struct {
	call func(context.Context) error
}

func (b readinessBackend) Call(ctx context.Context, method bridge.Method,
	payload json.RawMessage) (any, error) {

	return nil, b.call(ctx)
}

func TestReadinessRetriesLagButRejectsMismatch(t *testing.T) {
	calls := 0
	backend := readinessBackend{call: func(context.Context) error {
		calls++
		switch calls {
		case 1:
			return bridge.ErrPending
		case 2:
			return status.Error(codes.Unavailable, "restarting")
		default:
			return nil
		}
	}}
	require.NoError(t, waitReady(t.Context(), backend, time.Millisecond))
	require.Equal(t, 3, calls)
	calls = 0
	backend.call = func(context.Context) error {
		calls++
		return bridge.ErrInvalid
	}
	err := waitReady(t.Context(), backend, time.Millisecond)
	require.ErrorIs(t, err, bridge.ErrInvalid)
	require.Equal(t, 1, calls)
}

func TestReadinessStopsAtStartupDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	backend := readinessBackend{call: func(context.Context) error {
		calls++
		return bridge.ErrPending
	}}
	err := waitReady(ctx, backend, time.Hour)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, calls)
}
