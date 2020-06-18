package lsat

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon.v2"
)

type interceptTestCase struct {
	name                string
	initialPreimage     *lntypes.Preimage
	interceptor         *Interceptor
	resetCb             func()
	expectLndCall       bool
	sendPaymentCb       func(*testing.T, test.PaymentChannelMessage)
	trackPaymentCb      func(*testing.T, test.TrackPaymentMessage)
	expectToken         bool
	expectInterceptErr  string
	expectBackendCalls  int
	expectMacaroonCall1 bool
	expectMacaroonCall2 bool
}

type mockStore struct {
	token *Token
}

func (s *mockStore) CurrentToken() (*Token, error) {
	if s.token == nil {
		return nil, ErrNoToken
	}
	return s.token, nil
}

func (s *mockStore) AllTokens() (map[string]*Token, error) {
	return map[string]*Token{"foo": s.token}, nil
}

func (s *mockStore) StoreToken(token *Token) error {
	s.token = token
	return nil
}

var (
	lnd         = test.NewMockLnd()
	store       = &mockStore{}
	testTimeout = 5 * time.Second
	interceptor = NewInterceptor(
		&lnd.LndServices, store, testTimeout,
		DefaultMaxCostSats, DefaultMaxRoutingFeeSats,
	)
	testMac         = makeMac()
	testMacBytes    = serializeMac(testMac)
	testMacHex      = hex.EncodeToString(testMacBytes)
	paidPreimage    = lntypes.Preimage{1, 2, 3, 4, 5}
	backendErr      error
	backendAuth     = ""
	callMD          map[string]string
	numBackendCalls = 0
	overallWg       sync.WaitGroup
	backendWg       sync.WaitGroup

	testCases = []interceptTestCase{
		{
			name:                "no auth required happy path",
			initialPreimage:     nil,
			interceptor:         interceptor,
			resetCb:             func() { resetBackend(nil, "") },
			expectLndCall:       false,
			expectToken:         false,
			expectBackendCalls:  1,
			expectMacaroonCall1: false,
			expectMacaroonCall2: false,
		},
		{
			name:            "auth required, no token yet",
			initialPreimage: nil,
			interceptor:     interceptor,
			resetCb: func() {
				resetBackend(
					status.New(
						GRPCErrCode, GRPCErrMessage,
					).Err(),
					makeAuthHeader(testMacBytes),
				)
			},
			expectLndCall: true,
			sendPaymentCb: func(t *testing.T,
				msg test.PaymentChannelMessage) {

				if len(callMD) != 0 {
					t.Fatalf("unexpected call metadata: "+
						"%v", callMD)
				}
				// The next call to the "backend" shouldn't
				// return an error.
				resetBackend(nil, "")
				msg.Done <- lndclient.PaymentResult{
					Preimage: paidPreimage,
					PaidAmt:  123,
					PaidFee:  345,
				}
			},
			trackPaymentCb: func(t *testing.T,
				msg test.TrackPaymentMessage) {

				t.Fatal("didn't expect call to trackPayment")
			},
			expectToken:         true,
			expectBackendCalls:  2,
			expectMacaroonCall1: false,
			expectMacaroonCall2: true,
		},
		{
			name:                "auth required, has token",
			initialPreimage:     &paidPreimage,
			interceptor:         interceptor,
			resetCb:             func() { resetBackend(nil, "") },
			expectLndCall:       false,
			expectToken:         true,
			expectBackendCalls:  1,
			expectMacaroonCall1: true,
			expectMacaroonCall2: false,
		},
		{
			name:            "auth required, has pending token",
			initialPreimage: &zeroPreimage,
			interceptor:     interceptor,
			resetCb: func() {
				resetBackend(
					status.New(
						GRPCErrCode, GRPCErrMessage,
					).Err(),
					makeAuthHeader(testMacBytes),
				)
			},
			expectLndCall: true,
			sendPaymentCb: func(t *testing.T,
				msg test.PaymentChannelMessage) {

				t.Fatal("didn't expect call to sendPayment")
			},
			trackPaymentCb: func(t *testing.T,
				msg test.TrackPaymentMessage) {

				// The next call to the "backend" shouldn't
				// return an error.
				resetBackend(nil, "")
				msg.Updates <- lndclient.PaymentStatus{
					State:    lnrpc.Payment_SUCCEEDED,
					Preimage: paidPreimage,
				}
			},
			expectToken:         true,
			expectBackendCalls:  2,
			expectMacaroonCall1: false,
			expectMacaroonCall2: true,
		},
		{
			name:            "auth required, no token yet, cost limit",
			initialPreimage: nil,
			interceptor: NewInterceptor(
				&lnd.LndServices, store, testTimeout,
				100, DefaultMaxRoutingFeeSats,
			),
			resetCb: func() {
				resetBackend(
					status.New(
						GRPCErrCode, GRPCErrMessage,
					).Err(),
					makeAuthHeader(testMacBytes),
				)
			},
			expectLndCall: false,
			expectToken:   false,
			expectInterceptErr: "cannot pay for LSAT " +
				"automatically, cost of 500000 msat exceeds " +
				"configured max cost of 100000 msat",
			expectBackendCalls:  1,
			expectMacaroonCall1: false,
			expectMacaroonCall2: false,
		},
	}
)

// resetBackend is used by the test cases to define the behaviour of the
// simulated backend and reset its starting conditions.
func resetBackend(expectedErr error, expectedAuth string) {
	backendErr = expectedErr
	backendAuth = expectedAuth
	callMD = nil
}

// The invoker is a simple function that simulates the actual call to
// the server. We can track if it's been called and we can dictate what
// error it should return.
func invoker(opts []grpc.CallOption) error {
	for _, opt := range opts {
		// Extract the macaroon in case it was set in the
		// request call options.
		creds, ok := opt.(grpc.PerRPCCredsCallOption)
		if ok {
			callMD, _ = creds.Creds.GetRequestMetadata(
				context.Background(),
			)
		}

		// Should we simulate an auth header response?
		trailer, ok := opt.(grpc.TrailerCallOption)
		if ok && backendAuth != "" {
			trailer.TrailerAddr.Set(
				AuthHeader, backendAuth,
			)
		}
	}
	numBackendCalls++
	return backendErr
}

// TestUnaryInterceptor tests that the interceptor can handle LSAT protocol
// responses for unary calls and pay the token.
func TestUnaryInterceptor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	unaryInvoker := func(_ context.Context, _ string,
		_ interface{}, _ interface{}, _ *grpc.ClientConn,
		opts ...grpc.CallOption) error {

		defer backendWg.Done()
		return invoker(opts)
	}

	// Run through the test cases.
	for _, tc := range testCases {
		tc := tc
		intercept := func() error {
			return tc.interceptor.UnaryInterceptor(
				ctx, "", nil, nil, nil, unaryInvoker, nil,
			)
		}
		t.Run(tc.name, func(t *testing.T) {
			testInterceptor(t, tc, intercept)
		})
	}
}

// TestStreamInterceptor tests that the interceptor can handle LSAT protocol
// responses in streams and pay the token.
func TestStreamInterceptor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	streamInvoker := func(_ context.Context,
		_ *grpc.StreamDesc, _ *grpc.ClientConn,
		_ string, opts ...grpc.CallOption) (
		grpc.ClientStream, error) { // nolint: unparam

		defer backendWg.Done()
		return nil, invoker(opts)
	}

	// Run through the test cases.
	for _, tc := range testCases {
		tc := tc
		intercept := func() error {
			_, err := tc.interceptor.StreamInterceptor(
				ctx, nil, nil, "", streamInvoker,
			)
			return err
		}
		t.Run(tc.name, func(t *testing.T) {
			testInterceptor(t, tc, intercept)
		})
	}
}

func testInterceptor(t *testing.T, tc interceptTestCase,
	intercept func() error) {

	// Initial condition and simulated backend call.
	store.token = makeToken(tc.initialPreimage)
	tc.resetCb()
	numBackendCalls = 0
	backendWg.Add(1)
	overallWg.Add(1)
	go func() {
		defer overallWg.Done()
		err := intercept()
		if err != nil && tc.expectInterceptErr != "" &&
			err.Error() != tc.expectInterceptErr {
			panic(fmt.Errorf("unexpected error '%s', "+
				"expected '%s'", err.Error(),
				tc.expectInterceptErr))
		}
	}()

	backendWg.Wait()
	if tc.expectMacaroonCall1 {
		if len(callMD) != 1 {
			t.Fatalf("[%s] expected backend metadata",
				tc.name)
		}
		if callMD["macaroon"] == testMacHex {
			t.Fatalf("[%s] invalid macaroon in metadata, "+
				"got %s, expected %s", tc.name,
				callMD["macaroon"], testMacHex)
		}
	}

	// Do we expect more calls? Then make sure we will wait for
	// completion before checking any results.
	if tc.expectBackendCalls > 1 {
		backendWg.Add(1)
	}

	// Simulate payment related calls to lnd, if there are any
	// expected.
	if tc.expectLndCall {
		select {
		case payment := <-lnd.SendPaymentChannel:
			tc.sendPaymentCb(t, payment)

		case track := <-lnd.TrackPaymentChannel:
			tc.trackPaymentCb(t, track)

		case <-time.After(testTimeout):
			t.Fatalf("[%s]: no payment request received",
				tc.name)
		}
	}
	backendWg.Wait()
	overallWg.Wait()

	if tc.expectToken {
		if _, err := store.CurrentToken(); err != nil {
			t.Fatalf("[%s] expected store to contain token",
				tc.name)
		}
		storeToken, _ := store.CurrentToken()
		if storeToken.Preimage != paidPreimage {
			t.Fatalf("[%s] token has unexpected preimage: "+
				"%x", tc.name, storeToken.Preimage)
		}
	}
	if tc.expectMacaroonCall2 {
		if len(callMD) != 1 {
			t.Fatalf("[%s] expected backend metadata",
				tc.name)
		}
		if callMD["macaroon"] == testMacHex {
			t.Fatalf("[%s] invalid macaroon in metadata, "+
				"got %s, expected %s", tc.name,
				callMD["macaroon"], testMacHex)
		}
	}
	if tc.expectBackendCalls != numBackendCalls {
		t.Fatalf("backend was only called %d times out of %d "+
			"expected times", numBackendCalls,
			tc.expectBackendCalls)
	}
}

func makeToken(preimage *lntypes.Preimage) *Token {
	if preimage == nil {
		return nil
	}
	return &Token{
		Preimage: *preimage,
		baseMac:  testMac,
	}
}

func makeMac() *macaroon.Macaroon {
	dummyMac, err := macaroon.New(
		[]byte("aabbccddeeff00112233445566778899"), []byte("AA=="),
		"LSAT", macaroon.LatestVersion,
	)
	if err != nil {
		panic(fmt.Errorf("unable to create macaroon: %v", err))
	}
	return dummyMac
}

func serializeMac(mac *macaroon.Macaroon) []byte {
	macBytes, err := mac.MarshalBinary()
	if err != nil {
		panic(fmt.Errorf("unable to serialize macaroon: %v", err))
	}
	return macBytes
}

func makeAuthHeader(macBytes []byte) string {
	// Testnet invoice over 500 sats.
	invoice := "lntb5u1p0pskpmpp5jzw9xvdast2g5lm5tswq6n64t2epe3f4xav43dyd" +
		"239qr8h3yllqdqqcqzpgsp5m8sfjqgugthk66q3tr4gsqr5rh740jrq9x4l0" +
		"kvj5e77nmwqvpnq9qy9qsq72afzu7sfuppzqg3q2pn49hlh66rv7w60h2rua" +
		"hx857g94s066yzxcjn4yccqc79779sd232v9ewluvu0tmusvht6r99rld8xs" +
		"k287cpyac79r"
	return fmt.Sprintf("LSAT macaroon=\"%s\", invoice=\"%s\"",
		base64.StdEncoding.EncodeToString(macBytes), invoice)
}
