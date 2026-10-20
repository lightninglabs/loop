package loop

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightninglabs/aperture/l402"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestServerAuthDeadlineWhilePeerStalls verifies both interceptors share the
// same cancelable gate. An optional recovery call must time out while an older
// authenticated request is still stalled inside the actual L402 interceptor.
func TestServerAuthDeadlineWhilePeerStalls(t *testing.T) {
	for _, firstStream := range []bool{false, true} {
		for _, nextStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/queued-stream=%t",
				firstStream, nextStream), func(t *testing.T) {

				testServerAuthBlocked(
					t, firstStream, nextStream,
				)
			})
		}
	}
}

func testServerAuthBlocked(t *testing.T, firstStream, nextStream bool) {
	t.Parallel()
	store, err := l402.NewFileStore(t.TempDir())
	require.NoError(t, err)
	auth := newServerAuth(l402.NewInterceptor(nil, store,
		5*time.Second, 0, 0, true))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	blocked := func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	go func() { done <- callServerAuth(auth, ctx, firstStream, blocked) }()
	<-entered
	short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	var invoked atomic.Bool
	markInvoked := func(context.Context) error {
		invoked.Store(true)
		return nil
	}
	started := time.Now()
	err = callServerAuth(auth, short, nextStream, markInvoked)
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
	require.Less(t, time.Since(started), time.Second)
	require.False(t, invoked.Load())
	require.NoError(t, ctx.Err())
	unblock()
	require.NoError(t, <-done)
	require.NoError(t, callServerAuth(auth, ctx, nextStream,
		func(context.Context) error { return nil }))

	canceled, stop := context.WithCancel(ctx)
	stop()
	err = callServerAuth(auth, canceled, nextStream, markInvoked)
	require.Equal(t, codes.Canceled, status.Code(err))
	require.False(t, invoked.Load())
}

func callServerAuth(auth *serverAuth, ctx context.Context, stream bool,
	body func(context.Context) error) error {

	if stream {
		_, err := auth.stream(ctx, &grpc.StreamDesc{},
			nil, "/test/stream", func(ctx context.Context,
				_ *grpc.StreamDesc, _ *grpc.ClientConn,
				_ string, _ ...grpc.CallOption) (
				grpc.ClientStream, error) {

				return nil, body(ctx)
			})
		return err
	}
	return auth.unary(ctx, "/test/unary", nil, nil, nil,
		func(ctx context.Context, _ string, _, _ any,
			_ *grpc.ClientConn, _ ...grpc.CallOption) error {

			return body(ctx)
		})
}
