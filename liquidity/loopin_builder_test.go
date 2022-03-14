package liquidity

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestLoopinInUse tests that the loop in swap builder prevents dispatching
// swaps for peers when there is already a swap running for that peer.
func TestLoopinInUse(t *testing.T) {
	var (
		peer1 = route.Vertex{1}
		chan1 = lnwire.NewShortChanIDFromInt(1)

		peer2 = route.Vertex{2}
		chan2 = lnwire.NewShortChanIDFromInt(2)
	)

	tests := []struct {
		name           string
		ongoingLoopOut *lnwire.ShortChannelID
		ongoingLoopIn  *route.Vertex
		failedLoopIn   *route.Vertex
		expectedErr    error
	}{
		{
			name:           "swap allowed",
			ongoingLoopIn:  &peer2,
			ongoingLoopOut: &chan2,
			failedLoopIn:   &peer2,
			expectedErr:    nil,
		},
		{
			name:           "conflicts with loop out",
			ongoingLoopOut: &chan1,
			expectedErr:    newReasonError(ReasonLoopOut),
		},
		{
			name:          "conflicts with loop in",
			ongoingLoopIn: &peer1,
			expectedErr:   newReasonError(ReasonLoopIn),
		},
		{
			name:         "previous failed loopin",
			failedLoopIn: &peer1,
			expectedErr:  newReasonError(ReasonFailureBackoff),
		},
	}

	for _, testCase := range tests {
		traffic := newSwapTraffic()

		if testCase.ongoingLoopOut != nil {
			traffic.ongoingLoopOut[*testCase.ongoingLoopOut] = true
		}

		if testCase.ongoingLoopIn != nil {
			traffic.ongoingLoopIn[*testCase.ongoingLoopIn] = true
		}

		if testCase.failedLoopIn != nil {
			traffic.failedLoopIn[*testCase.failedLoopIn] = testTime
		}

		builder := newLoopInBuilder(nil)
		err := builder.inUse(traffic, peer1, []lnwire.ShortChannelID{
			chan1,
		})

		require.Equal(t, testCase.expectedErr, err)
	}
}

// TestLoopinBuildSwap tests construction of loop in swaps for autoloop,
// including the case where the client cannot get a quote because it is not
// reachable from the server.
func TestLoopinBuildSwap(t *testing.T) {
	var (
		peer1 = route.Vertex{1}
		chan1 = lnwire.NewShortChanIDFromInt(1)

		htlcConfTarget int32          = 6
		swapAmt        btcutil.Amount = 100000

		quote = &loop.LoopInQuote{
			SwapFee:  1,
			MinerFee: 2,
		}

		expectedSwap = &loopInSwapSuggestion{
			loop.LoopInRequest{
				Amount:         swapAmt,
				MaxSwapFee:     quote.SwapFee,
				MaxMinerFee:    quote.MinerFee,
				HtlcConfTarget: htlcConfTarget,
				LastHop:        &peer1,
				Initiator:      autoloopSwapInitiator,
			},
		}

		quoteRequest = &loop.LoopInQuoteRequest{
			Amount:         swapAmt,
			LastHop:        &peer1,
			HtlcConfTarget: htlcConfTarget,
		}

		errPrecondition = status.Error(codes.FailedPrecondition, "failed")
		errOtherCode    = status.Error(codes.DeadlineExceeded, "timeout")
		errNoCode       = errors.New("failure")
	)

	tests := []struct {
		name         string
		prepareMock  func(*mockCfg)
		expectedSwap swapSuggestion
		expectedErr  error
	}{
		{
			name: "quote successful",
			prepareMock: func(m *mockCfg) {
				m.On(
					"LoopInQuote", mock.Anything,
					quoteRequest,
				).Return(quote, nil)
			},
			expectedSwap: expectedSwap,
		},
		{
			name: "client unreachable",
			prepareMock: func(m *mockCfg) {
				m.On(
					"LoopInQuote", mock.Anything,
					quoteRequest,
				).Return(quote, errPrecondition)
			},
			expectedSwap: nil,
			expectedErr:  newReasonError(ReasonLoopInUnreachable),
		},
		{
			name: "other error code",
			prepareMock: func(m *mockCfg) {
				m.On(
					"LoopInQuote", mock.Anything,
					quoteRequest,
				).Return(quote, errOtherCode)
			},
			expectedSwap: nil,
			expectedErr:  errOtherCode,
		},
		{
			name: "no error code",
			prepareMock: func(m *mockCfg) {
				m.On(
					"LoopInQuote", mock.Anything,
					quoteRequest,
				).Return(quote, errNoCode)
			},
			expectedSwap: nil,
			expectedErr:  errNoCode,
		},
	}

	for _, testCase := range tests {
		mock, cfg := newMockConfig()
		params := defaultParameters
		params.HtlcConfTarget = htlcConfTarget
		params.AutoFeeBudget = 100000

		testCase.prepareMock(mock)

		builder := newLoopInBuilder(cfg)
		swap, err := builder.buildSwap(
			context.Background(), peer1, []lnwire.ShortChannelID{
				chan1,
			}, swapAmt, false, params,
		)
		assert.Equal(t, testCase.expectedSwap, swap)
		assert.Equal(t, testCase.expectedErr, err)

		mock.AssertExpectations(t)
	}
}
