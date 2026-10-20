package loop

import (
	"context"

	"github.com/lightninglabs/aperture/l402"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// serverAuth preserves L402's serialized acquisition without making a queued
// caller wait beyond its deadline. Every entry to the shared L402 interceptor
// passes this gate, so its internal non-cancelable mutex remains uncontended.
// The stream gate covers establishment only, matching the L402 interceptor.
type serverAuth struct {
	interceptor *l402.ClientInterceptor
	gate        chan struct{}
}

func newServerAuth(interceptor *l402.ClientInterceptor) *serverAuth {
	return &serverAuth{
		interceptor: interceptor, gate: make(chan struct{}, 1),
	}
}

func (a *serverAuth) acquire(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	case a.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-a.gate
			return nil, status.FromContextError(err).Err()
		}
		return func() { <-a.gate }, nil
	}
}

func (a *serverAuth) unary(ctx context.Context, method string, request,
	response any, conn *grpc.ClientConn, invoke grpc.UnaryInvoker,
	opts ...grpc.CallOption) error {

	release, err := a.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return a.interceptor.UnaryInterceptor(ctx, method, request, response,
		conn, invoke, opts...)
}

func (a *serverAuth) stream(ctx context.Context, desc *grpc.StreamDesc,
	conn *grpc.ClientConn, method string, open grpc.Streamer,
	opts ...grpc.CallOption) (grpc.ClientStream, error) {

	release, err := a.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return a.interceptor.StreamInterceptor(ctx, desc, conn, method, open,
		opts...)
}
