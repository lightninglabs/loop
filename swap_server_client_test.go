package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/aperture/l402"
	looptest "github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon.v2"
)

// TestL402ClientInterceptorConcurrentPaidCalls verifies that calls using an
// existing paid token reach the transport concurrently.
func TestL402ClientInterceptorConcurrentPaidCalls(t *testing.T) {
	t.Parallel()

	paidMacaroon, err := macaroon.New(
		[]byte("root key"), []byte("token id"), "test",
		macaroon.LatestVersion,
	)
	require.NoError(t, err)

	interceptor := &l402ClientInterceptor{
		callTimeout: time.Minute,
		loadPaidMacaroon: func() (*macaroon.Macaroon, bool, error) {
			return paidMacaroon, true, nil
		},
		fallbackUnary: func(context.Context, string, any, any,
			*grpc.ClientConn, grpc.UnaryInvoker,
			...grpc.CallOption) error {

			return errors.New("unexpected L402 fallback")
		},
	}

	callStarted := make(chan int, 2)
	releaseCalls := make(chan struct{})
	invoker := func(_ context.Context, _ string, req, _ any,
		_ *grpc.ClientConn, opts ...grpc.CallOption) error {

		// A paid call must carry the original option plus the L402
		// credential before it reaches the transport.
		if len(opts) != 2 {
			return errors.New("paid call has unexpected " +
				"call options")
		}

		callStarted <- req.(int)
		<-releaseCalls

		return nil
	}

	callErr := make(chan error, 2)
	for id := 1; id <= 2; id++ {
		go func() {
			callErr <- interceptor.UnaryInterceptor(
				t.Context(), "test", id, nil, nil, invoker,
				grpc.WaitForReady(true),
			)
		}()
	}

	// Both calls must arrive while the first remains blocked. This is the
	// property the Aperture interceptor's unconditional mutex prevented.
	started := map[int]bool{
		receiveOrTimeout(t, callStarted): true,
		receiveOrTimeout(t, callStarted): true,
	}
	require.Equal(t, map[int]bool{1: true, 2: true}, started)

	close(releaseCalls)
	require.NoError(t, receiveOrTimeout(t, callErr))
	require.NoError(t, receiveOrTimeout(t, callErr))
}

// TestL402ClientInterceptorPaymentChallengeFallback verifies that a challenge
// on the concurrent path is retried through serialized token handling.
func TestL402ClientInterceptorPaymentChallengeFallback(t *testing.T) {
	t.Parallel()

	paidMacaroon, err := macaroon.New(
		[]byte("root key"), []byte("token id"), "test",
		macaroon.LatestVersion,
	)
	require.NoError(t, err)

	fallbackCalled := make(chan struct{}, 1)
	interceptor := &l402ClientInterceptor{
		callTimeout: time.Minute,
		loadPaidMacaroon: func() (*macaroon.Macaroon, bool, error) {
			return paidMacaroon, true, nil
		},
		fallbackUnary: func(context.Context, string, any, any,
			*grpc.ClientConn, grpc.UnaryInvoker,
			...grpc.CallOption) error {

			fallbackCalled <- struct{}{}

			return nil
		},
	}

	invoker := func(context.Context, string, any, any,
		*grpc.ClientConn, ...grpc.CallOption) error {

		// Simulate the server rejecting the cached paid token so the
		// payment-aware fallback owns any acquisition or retry.
		return status.Error(l402.GRPCErrCode, l402.GRPCErrMessage)
	}

	err = interceptor.UnaryInterceptor(
		t.Context(), "test", nil, nil, nil, invoker,
	)
	require.NoError(t, err)
	receiveOrTimeout(t, fallbackCalled)
}

// TestL402ClientInterceptorCurrentTokenFallback verifies that token states
// which cannot use the concurrent paid path enter serialized token handling.
func TestL402ClientInterceptorCurrentTokenFallback(t *testing.T) {
	t.Parallel()

	storeErr := errors.New("token store failed")
	testCases := []struct {
		name     string
		token    *l402.Token
		tokenErr error
	}{
		{
			name:     "no token",
			tokenErr: l402.ErrNoToken,
		},
		{
			name:  "pending token",
			token: &l402.Token{},
		},
		{
			name: "nil token",
		},
		{
			name:     "store error",
			tokenErr: storeErr,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fallbackCalled := false
			interceptor := &l402ClientInterceptor{
				tokenStore: &staticL402Store{
					token: testCase.token,
					err:   testCase.tokenErr,
				},
				fallbackUnary: func(context.Context, string,
					any, any, *grpc.ClientConn,
					grpc.UnaryInvoker,
					...grpc.CallOption) error {

					// Reaching this callback proves the
					// unusable token did not enter the
					// concurrent transport path.
					fallbackCalled = true

					return nil
				},
			}
			interceptor.loadPaidMacaroon =
				interceptor.currentPaidMacaroon

			invoker := func(context.Context, string, any, any,
				*grpc.ClientConn, ...grpc.CallOption) error {

				return errors.New("unexpected concurrent call")
			}

			err := interceptor.UnaryInterceptor(
				t.Context(), "test", nil, nil, nil, invoker,
			)
			require.NoError(t, err)
			require.True(t, fallbackCalled)
		})
	}
}

// receiveOrTimeout returns the next channel value or fails after one second.
func receiveOrTimeout[T any](t *testing.T, values <-chan T) T {
	t.Helper()

	select {
	case value := <-values:
		return value

	case <-time.After(time.Second):
		var zero T
		t.Fatal("timed out waiting for test value")

		return zero
	}
}

// staticL402Store returns a configured token result for interceptor tests.
type staticL402Store struct {
	// Store supplies methods unused by the token-loading tests.
	l402.Store

	// token is returned by CurrentToken.
	token *l402.Token

	// err is returned by CurrentToken.
	err error
}

// CurrentToken returns the token result configured by the test.
func (s *staticL402Store) CurrentToken() (*l402.Token, error) {
	return s.token, s.err
}

// TestParseServerPubKey ensures that parseServerPubKey accepts a valid
// compressed public key and rejects keys with an invalid length or contents.
func TestParseServerPubKey(t *testing.T) {
	t.Parallel()

	_, pubKey := looptest.CreateKey(1)
	pubKeyBytes := pubKey.SerializeCompressed()

	parsedKey, err := parseServerPubKey("test key", pubKeyBytes)
	require.NoError(t, err)
	require.Equal(t, pubKeyBytes, parsedKey[:])

	_, err = parseServerPubKey("test key", pubKeyBytes[:32])
	require.ErrorContains(t, err, "invalid test key length")

	invalidKey := make([]byte, 33)
	_, err = parseServerPubKey("test key", invalidKey)
	require.ErrorContains(t, err, "invalid test key")
}
