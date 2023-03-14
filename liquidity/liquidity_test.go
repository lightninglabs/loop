package liquidity

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	clientrpc "github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	testTime        = time.Date(2020, 02, 13, 0, 0, 0, 0, time.UTC)
	testBudgetStart = testTime.Add(time.Hour * -1)
	// In order to not influence existing tests we set the budget refresh
	// period to 10 years so that it will never be refreshed. This way the
	// behavior of autoloop remains identical to before recurring budget was
	// introduced.
	testBudgetRefresh = time.Hour * 24 * 365 * 10

	chanID1 = lnwire.NewShortChanIDFromInt(1)
	chanID2 = lnwire.NewShortChanIDFromInt(2)
	chanID3 = lnwire.NewShortChanIDFromInt(3)

	peer1 = route.Vertex{1}
	peer2 = route.Vertex{2}

	channel1 = lndclient.ChannelInfo{
		ChannelID:     chanID1.ToUint64(),
		PubKeyBytes:   peer1,
		LocalBalance:  10000,
		RemoteBalance: 0,
		Capacity:      10000,
	}

	channel2 = lndclient.ChannelInfo{
		ChannelID:     chanID2.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  10000,
		RemoteBalance: 0,
		Capacity:      10000,
	}

	// chanRule is a rule that produces chan1Rec.
	chanRule = &SwapRule{
		ThresholdRule: NewThresholdRule(50, 0),
		Type:          swap.TypeOut,
	}

	testQuote = &loop.LoopOutQuote{
		SwapFee:      btcutil.Amount(5),
		PrepayAmount: btcutil.Amount(50),
		MinerFee:     btcutil.Amount(1),
	}

	prepayFee, routingFee = testPPMFees(defaultFeePPM, testQuote, 7500)

	// chan1Rec is the suggested swap for channel 1 when we use chanRule.
	chan1Rec = loop.OutRequest{
		Amount:              7500,
		OutgoingChanSet:     loopdb.ChannelSet{chanID1.ToUint64()},
		MaxPrepayRoutingFee: prepayFee,
		MaxSwapRoutingFee:   routingFee,
		MaxMinerFee:         scaleMinerFee(testQuote.MinerFee),
		MaxSwapFee:          testQuote.SwapFee,
		MaxPrepayAmount:     testQuote.PrepayAmount,
		SweepConfTarget:     defaultConfTarget,
		Initiator:           autoloopSwapInitiator,
	}

	// chan2Rec is the suggested swap for channel 2 when we use chanRule.
	chan2Rec = loop.OutRequest{
		Amount:              7500,
		OutgoingChanSet:     loopdb.ChannelSet{chanID2.ToUint64()},
		MaxPrepayRoutingFee: prepayFee,
		MaxSwapRoutingFee:   routingFee,
		MaxMinerFee:         scaleMinerFee(testQuote.MinerFee),
		MaxPrepayAmount:     testQuote.PrepayAmount,
		MaxSwapFee:          testQuote.SwapFee,
		SweepConfTarget:     defaultConfTarget,
		Initiator:           autoloopSwapInitiator,
	}

	// chan1Out is a contract that uses channel 1, used to represent on
	// disk swap using chan 1.
	chan1Out = &loopdb.LoopOutContract{
		OutgoingChanSet: loopdb.ChannelSet(
			[]uint64{
				chanID1.ToUint64(),
			},
		),
	}

	// autoOutContract is a contract for an existing loop out that was
	// automatically dispatched. This swap is within our test budget period,
	// and restricted to a channel that we do not use in our tests.
	autoOutContract = &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			Label:          labels.AutoloopLabel(swap.TypeOut),
			InitiationTime: testBudgetStart,
		},
		OutgoingChanSet: loopdb.ChannelSet{999},
	}

	autoInContract = &loopdb.LoopInContract{
		SwapContract: loopdb.SwapContract{
			Label:          labels.AutoloopLabel(swap.TypeIn),
			InitiationTime: testBudgetStart,
		},
	}

	testRestrictions = NewRestrictions(1, 10000)

	// noneDisqualified can be used in tests where we don't have any
	// disqualified channels so that we can use require.Equal.
	noneDisqualified = make(map[lnwire.ShortChannelID]Reason)

	// noPeersDisqualified can be used in tests where we don't have any
	// disqualified peers so that we can use require.Equal.
	noPeersDisqualified = make(map[route.Vertex]Reason)
)

// newTestConfig creates a default test config.
func newTestConfig() (*Config, *test.LndMockServices) {
	lnd := test.NewMockLnd()

	// Set our fee estimate for the default number of confirmations to our
	// limit so that our fees will be ok by default.
	lnd.SetFeeEstimate(
		defaultParameters.SweepConfTarget, defaultSweepFeeRateLimit,
	)

	return &Config{
		Restrictions: func(_ context.Context, _ swap.Type) (*Restrictions,
			error) {

			return testRestrictions, nil
		},
		Lnd:   &lnd.LndServices,
		Clock: clock.NewTestClock(testTime),
		ListLoopOut: func() ([]*loopdb.LoopOut, error) {
			return nil, nil
		},
		ListLoopIn: func() ([]*loopdb.LoopIn, error) {
			return nil, nil
		},
		LoopOutQuote: func(_ context.Context,
			_ *loop.LoopOutQuoteRequest) (*loop.LoopOutQuote,
			error) {

			return testQuote, nil
		},
	}, lnd
}

// testPPMFees calculates the split of fees between prepay and swap invoice
// for the swap amount and ppm, relying on the test quote.
func testPPMFees(ppm uint64, quote *loop.LoopOutQuote,
	swapAmount btcutil.Amount) (btcutil.Amount, btcutil.Amount) {

	feeTotal := ppmToSat(swapAmount, ppm)
	feeAvailable := feeTotal - scaleMinerFee(quote.MinerFee) - quote.SwapFee

	return splitOffChain(
		feeAvailable, quote.PrepayAmount, swapAmount,
	)
}

// applyFeeCategoryQuote returns a copy of the loop out request provided with
// fee categories updated to the quote and routing settings provided.
// nolint:unparam
func applyFeeCategoryQuote(req loop.OutRequest, minerFee btcutil.Amount,
	prepayPPM, routingPPM uint64, quote loop.LoopOutQuote) loop.OutRequest {

	req.MaxPrepayRoutingFee = ppmToSat(quote.PrepayAmount, prepayPPM)
	req.MaxSwapRoutingFee = ppmToSat(req.Amount, routingPPM)
	req.MaxSwapFee = quote.SwapFee
	req.MaxPrepayAmount = quote.PrepayAmount
	req.MaxMinerFee = minerFee

	return req
}

// TestParameters tests getting and setting of parameters for our manager.
func TestParameters(t *testing.T) {
	cfg, _ := newTestConfig()
	manager := NewManager(cfg)

	chanID := lnwire.NewShortChanIDFromInt(1)

	// Start with the case where we have no rules set.
	startParams := manager.GetParameters()
	require.Equal(t, defaultParameters, startParams)

	// Mutate the parameters returned by our get function.
	startParams.ChannelRules[chanID] = &SwapRule{
		ThresholdRule: NewThresholdRule(1, 1),
		Type:          swap.TypeOut,
	}

	// Make sure that we have not mutated the liquidity manager's params
	// by making this change.
	params := manager.GetParameters()
	require.Equal(t, defaultParameters, params)

	// Provide a valid set of parameters and validate assert that they are
	// set.
	originalRule := &SwapRule{
		ThresholdRule: NewThresholdRule(10, 10),
		Type:          swap.TypeOut,
	}

	expected := defaultParameters
	expected.ChannelRules = map[lnwire.ShortChannelID]*SwapRule{
		chanID: originalRule,
	}

	err := manager.setParameters(context.Background(), expected)
	require.NoError(t, err)

	// Check that changing the parameters we just set does not mutate
	// our liquidity manager's parameters.
	expected.ChannelRules[chanID] = &SwapRule{
		ThresholdRule: NewThresholdRule(11, 11),
		Type:          swap.TypeOut,
	}

	params = manager.GetParameters()
	require.NoError(t, err)
	require.Equal(t, originalRule, params.ChannelRules[chanID])

	// Set invalid parameters and assert that we fail.
	expected.ChannelRules = map[lnwire.ShortChannelID]*SwapRule{
		lnwire.NewShortChanIDFromInt(0): {
			ThresholdRule: NewThresholdRule(1, 2),
			Type:          swap.TypeOut,
		},
	}
	err = manager.setParameters(context.Background(), expected)
	require.Equal(t, ErrZeroChannelID, err)
}

// TestPersistParams tests reading and writing of parameters for our manager.
func TestPersistParams(t *testing.T) {
	rpcParams := &clientrpc.LiquidityParameters{
		FeePpm:          100,
		AutoMaxInFlight: 10,
		HtlcConfTarget:  2,
	}
	cfg, _ := newTestConfig()
	manager := NewManager(cfg)

	var paramsBytes []byte

	// Mock the read method to return empty data.
	manager.cfg.FetchLiquidityParams = func() ([]byte, error) {
		return paramsBytes, nil
	}

	// Test the nil params is returned.
	req, err := manager.loadParams()
	require.Nil(t, req)
	require.NoError(t, err)

	// Mock the write method to return no error.
	manager.cfg.PutLiquidityParams = func(data []byte) error {
		paramsBytes = data
		return nil
	}

	// Test save the message.
	err = manager.saveParams(rpcParams)
	require.NoError(t, err)

	// Test the nil params is returned.
	req, err = manager.loadParams()
	require.NoError(t, err)

	// Check the specified fields are set as expected.
	require.Equal(t, rpcParams.FeePpm, req.FeePpm)
	require.Equal(t, rpcParams.AutoMaxInFlight, req.AutoMaxInFlight)
	require.Equal(t, rpcParams.HtlcConfTarget, req.HtlcConfTarget)

	// Check the unspecified fields are using empty values.
	require.False(t, req.Autoloop)
	require.Empty(t, req.Rules)
	require.Zero(t, req.AutoloopBudgetSat)

	// Finally, check the loaded request can be used to set params without
	// error.
	err = manager.SetParameters(context.Background(), req)
	require.NoError(t, err)
}

// TestRestrictedSuggestions tests getting of swap suggestions when we have
// other in-flight swaps. We setup our manager with a set of channels and rules
// that require a loop out swap, focusing on the filtering our of channels that
// are in use for in-flight swaps, or those which have recently failed.
func TestRestrictedSuggestions(t *testing.T) {
	var (
		failedWithinTimeout = &loopdb.LoopEvent{
			SwapStateData: loopdb.SwapStateData{
				State: loopdb.StateFailOffchainPayments,
			},
			Time: testTime,
		}

		failedBeforeBackoff = &loopdb.LoopEvent{
			SwapStateData: loopdb.SwapStateData{
				State: loopdb.StateFailOffchainPayments,
			},
			Time: testTime.Add(
				defaultFailureBackoff * -1,
			),
		}

		// failedTemporary is a swap that failed outside of our backoff
		// period, but we still want to back off because the swap is
		// considered pending.
		failedTemporary = &loopdb.LoopEvent{
			SwapStateData: loopdb.SwapStateData{
				State: loopdb.StateFailTemporary,
			},
			Time: testTime.Add(
				defaultFailureBackoff * -3,
			),
		}

		chanRules = map[lnwire.ShortChannelID]*SwapRule{
			chanID1: chanRule,
			chanID2: chanRule,
		}
	)

	tests := []struct {
		name      string
		channels  []lndclient.ChannelInfo
		loopOut   []*loopdb.LoopOut
		loopIn    []*loopdb.LoopIn
		chanRules map[lnwire.ShortChannelID]*SwapRule
		peerRules map[route.Vertex]*SwapRule
		expected  *Suggestions
	}{
		{
			name: "no existing swaps",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			loopOut:   nil,
			loopIn:    nil,
			chanRules: chanRules,
			expected: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1Rec,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "unrestricted loop out",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			loopOut: []*loopdb.LoopOut{
				{
					Contract: &loopdb.LoopOutContract{
						OutgoingChanSet: nil,
					},
				},
			},
			chanRules: chanRules,
			expected: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1Rec,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "unrestricted loop in",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			loopIn: []*loopdb.LoopIn{
				{
					Contract: &loopdb.LoopInContract{
						LastHop: nil,
					},
				},
			},
			chanRules: chanRules,
			expected: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1Rec,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "restricted loop out",
			channels: []lndclient.ChannelInfo{
				channel1, channel2,
			},
			loopOut: []*loopdb.LoopOut{
				{
					Contract: chan1Out,
				},
			},
			chanRules: chanRules,
			expected: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan2Rec,
				},
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonLoopOut,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "restricted loop in",
			channels: []lndclient.ChannelInfo{
				channel1, channel2,
			},
			loopIn: []*loopdb.LoopIn{
				{
					Contract: &loopdb.LoopInContract{
						LastHop: &peer2,
					},
				},
			},
			chanRules: chanRules,
			expected: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1Rec,
				},
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID2: ReasonLoopIn,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "swap failed recently",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			loopOut: []*loopdb.LoopOut{
				{
					Contract: chan1Out,
					Loop: loopdb.Loop{
						Events: []*loopdb.LoopEvent{
							failedWithinTimeout,
						},
					},
				},
			},
			chanRules: chanRules,
			expected: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonFailureBackoff,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "swap failed before cutoff",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			loopOut: []*loopdb.LoopOut{
				{
					Contract: chan1Out,
					Loop: loopdb.Loop{
						Events: []*loopdb.LoopEvent{
							failedBeforeBackoff,
						},
					},
				},
			},
			chanRules: chanRules,
			expected: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1Rec,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "temporary failure",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			loopOut: []*loopdb.LoopOut{
				{
					Contract: chan1Out,
					Loop: loopdb.Loop{
						Events: []*loopdb.LoopEvent{
							failedTemporary,
						},
					},
				},
			},
			chanRules: chanRules,
			expected: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonLoopOut,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "existing on peer's channel",
			channels: []lndclient.ChannelInfo{
				channel1,
				{
					ChannelID:   chanID3.ToUint64(),
					PubKeyBytes: peer1,
				},
			},
			loopOut: []*loopdb.LoopOut{
				{
					Contract: chan1Out,
				},
			},
			peerRules: map[route.Vertex]*SwapRule{
				peer1: {
					ThresholdRule: NewThresholdRule(0, 50),
					Type:          swap.TypeOut,
				},
			},
			expected: &Suggestions{
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: map[route.Vertex]Reason{
					peer1: ReasonLoopOut,
				},
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			// Create a manager config which will return the test
			// case's set of existing swaps.
			cfg, lnd := newTestConfig()
			cfg.ListLoopOut = func() ([]*loopdb.LoopOut, error) {
				return testCase.loopOut, nil
			}
			cfg.ListLoopIn = func() ([]*loopdb.LoopIn, error) {
				return testCase.loopIn, nil
			}

			lnd.Channels = testCase.channels

			params := defaultParameters
			params.AutoloopBudgetLastRefresh = testBudgetStart
			if testCase.chanRules != nil {
				params.ChannelRules = testCase.chanRules
			}

			if testCase.peerRules != nil {
				params.PeerRules = testCase.peerRules
			}

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.expected, nil,
			)
		})
	}
}

// TestSweepFeeLimit tests getting of swap suggestions when our estimated sweep
// fee is above and below the configured limit.
func TestSweepFeeLimit(t *testing.T) {
	quote := &loop.LoopOutQuote{
		SwapFee:      btcutil.Amount(1),
		PrepayAmount: btcutil.Amount(500),
		MinerFee:     btcutil.Amount(50),
	}

	tests := []struct {
		name        string
		feeRate     chainfee.SatPerKWeight
		suggestions *Suggestions
	}{
		{
			name:    "fee estimate ok",
			feeRate: defaultSweepFeeRateLimit,
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					applyFeeCategoryQuote(
						chan1Rec, defaultMaximumMinerFee,
						defaultPrepayRoutingFeePPM,
						defaultRoutingFeePPM, *quote,
					),
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:    "fee estimate above limit",
			feeRate: defaultSweepFeeRateLimit + 1,
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonSweepFees,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()

			cfg.LoopOutQuote = func(_ context.Context,
				_ *loop.LoopOutQuoteRequest) (*loop.LoopOutQuote,
				error) {

				return quote, nil
			}

			// Set our test case's fee rate for our mock lnd.
			lnd.SetFeeEstimate(
				defaultConfTarget, testCase.feeRate,
			)

			lnd.Channels = []lndclient.ChannelInfo{
				channel1,
			}

			params := defaultParameters
			params.AutoloopBudgetLastRefresh = testBudgetStart
			params.FeeLimit = defaultFeeCategoryLimit()

			// Set our budget to cover a single swap with these
			// parameters.
			params.AutoFeeBudget = defaultMaximumMinerFee +
				ppmToSat(7500, defaultSwapFeePPM) +
				ppmToSat(7500, defaultPrepayRoutingFeePPM) +
				ppmToSat(7500, defaultRoutingFeePPM)

			params.ChannelRules = map[lnwire.ShortChannelID]*SwapRule{
				chanID1: chanRule,
			}

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.suggestions, nil,
			)
		})
	}
}

// TestSuggestSwaps tests getting of swap suggestions based on the rules set for
// the liquidity manager and the current set of channel balances.
func TestSuggestSwaps(t *testing.T) {
	singleChannel := []lndclient.ChannelInfo{
		channel1,
	}

	expectedAmt := btcutil.Amount(10000)
	prepay, routing := testPPMFees(defaultFeePPM, testQuote, expectedAmt)

	tests := []struct {
		name        string
		channels    []lndclient.ChannelInfo
		rules       map[lnwire.ShortChannelID]*SwapRule
		peerRules   map[route.Vertex]*SwapRule
		suggestions *Suggestions
		err         error
	}{
		{
			name:     "no rules",
			channels: singleChannel,
			rules:    map[lnwire.ShortChannelID]*SwapRule{},
			err:      ErrNoRules,
		},
		{
			name:     "loop out",
			channels: singleChannel,
			rules: map[lnwire.ShortChannelID]*SwapRule{
				chanID1: chanRule,
			},
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1Rec,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:     "no rule for channel",
			channels: singleChannel,
			rules: map[lnwire.ShortChannelID]*SwapRule{
				chanID2: {
					ThresholdRule: NewThresholdRule(10, 10),
					Type:          swap.TypeOut,
				},
			},
			suggestions: &Suggestions{
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "multiple peer rules",
			channels: []lndclient.ChannelInfo{
				{
					PubKeyBytes:   peer1,
					ChannelID:     chanID1.ToUint64(),
					Capacity:      20000,
					LocalBalance:  8000,
					RemoteBalance: 12000,
				},
				{
					PubKeyBytes:   peer1,
					ChannelID:     chanID2.ToUint64(),
					Capacity:      10000,
					LocalBalance:  9000,
					RemoteBalance: 1000,
				},
				{
					PubKeyBytes:   peer2,
					ChannelID:     chanID3.ToUint64(),
					Capacity:      5000,
					LocalBalance:  2000,
					RemoteBalance: 3000,
				},
			},
			peerRules: map[route.Vertex]*SwapRule{
				peer1: {
					ThresholdRule: NewThresholdRule(80, 0),
					Type:          swap.TypeOut,
				},
				peer2: {
					ThresholdRule: NewThresholdRule(40, 50),
					Type:          swap.TypeOut,
				},
			},
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					{
						Amount: expectedAmt,
						OutgoingChanSet: loopdb.ChannelSet{
							chanID1.ToUint64(),
							chanID2.ToUint64(),
						},
						MaxPrepayRoutingFee: prepay,
						MaxSwapRoutingFee:   routing,
						MaxMinerFee:         scaleMinerFee(testQuote.MinerFee),
						MaxSwapFee:          testQuote.SwapFee,
						MaxPrepayAmount:     testQuote.PrepayAmount,
						SweepConfTarget:     defaultConfTarget,
						Initiator:           autoloopSwapInitiator,
					},
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: map[route.Vertex]Reason{
					peer2: ReasonLiquidityOk,
				},
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()

			lnd.Channels = testCase.channels

			params := defaultParameters
			params.AutoloopBudgetLastRefresh = testBudgetStart
			if testCase.rules != nil {
				params.ChannelRules = testCase.rules
			}

			if testCase.peerRules != nil {
				params.PeerRules = testCase.peerRules
			}

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.suggestions, testCase.err,
			)
		})
	}
}

// TestFeeLimits tests limiting of swap suggestions by fees.
func TestFeeLimits(t *testing.T) {
	quote := &loop.LoopOutQuote{
		SwapFee:      btcutil.Amount(1),
		PrepayAmount: btcutil.Amount(500),
		MinerFee:     btcutil.Amount(50),
	}

	tests := []struct {
		name        string
		quote       *loop.LoopOutQuote
		suggestions *Suggestions
	}{
		{
			name:  "fees ok",
			quote: quote,
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					applyFeeCategoryQuote(
						chan1Rec, defaultMaximumMinerFee,
						defaultPrepayRoutingFeePPM,
						defaultRoutingFeePPM, *quote,
					),
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "insufficient prepay",
			quote: &loop.LoopOutQuote{
				SwapFee:      1,
				PrepayAmount: defaultMaximumPrepay + 1,
				MinerFee:     50,
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonPrepay,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "insufficient miner fee",
			quote: &loop.LoopOutQuote{
				SwapFee:      1,
				PrepayAmount: 100,
				MinerFee:     defaultMaximumMinerFee + 1,
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonMinerFee,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			// Swap fee limited to 0.5% of 7500 = 37,5.
			name: "insufficient swap fee",
			quote: &loop.LoopOutQuote{
				SwapFee:      38,
				PrepayAmount: 100,
				MinerFee:     500,
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonSwapFee,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()
			cfg.LoopOutQuote = func(context.Context,
				*loop.LoopOutQuoteRequest) (*loop.LoopOutQuote,
				error) {

				return testCase.quote, nil
			}

			lnd.Channels = []lndclient.ChannelInfo{
				channel1,
			}

			// Set our params to use individual fee limits.
			params := defaultParameters
			params.AutoloopBudgetLastRefresh = testBudgetStart
			params.FeeLimit = defaultFeeCategoryLimit()

			// Set our budget to cover a single swap with these
			// parameters.
			params.AutoFeeBudget = defaultMaximumMinerFee +
				ppmToSat(7500, defaultSwapFeePPM) +
				ppmToSat(7500, defaultPrepayRoutingFeePPM) +
				ppmToSat(7500, defaultRoutingFeePPM)

			params.ChannelRules = map[lnwire.ShortChannelID]*SwapRule{
				chanID1: chanRule,
			}

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.suggestions, nil,
			)
		})
	}
}

// TestFeeBudget tests limiting of swap suggestions to a fee budget, with and
// without existing swaps. This test uses example channels and rules which need
// a 7500 sat loop out. With our default parameters, and our test quote with
// a prepay of 500, our total fees are (rounded due to int multiplication):
// swap fee: 1 (as set in test quote)
// route fee: 7500 * 0.005 = 37
// prepay route: 500 * 0.005 = 2 sat
// max miner: set by default params
// Since our routing fees are calculated as a portion of our swap/prepay
// amounts, we use our max miner fee to shift swap cost to values above/below
// our budget, fixing our other fees at 114 sat for simplicity.
func TestFeeBudget(t *testing.T) {
	quote := &loop.LoopOutQuote{
		SwapFee:      btcutil.Amount(1),
		PrepayAmount: btcutil.Amount(500),
		MinerFee:     btcutil.Amount(50),
	}

	chan1 := applyFeeCategoryQuote(
		chan1Rec, 5000, defaultPrepayRoutingFeePPM,
		defaultRoutingFeePPM, *quote,
	)
	chan2 := applyFeeCategoryQuote(
		chan2Rec, 5000, defaultPrepayRoutingFeePPM,
		defaultRoutingFeePPM, *quote,
	)

	tests := []struct {
		name string

		// budget is our autoloop budget.
		budget btcutil.Amount

		// maxMinerFee is the maximum miner fee we will pay for swaps.
		maxMinerFee btcutil.Amount

		// existingSwaps represents our existing swaps, mapping their
		// last update time to their total cost.
		existingSwaps map[time.Time]btcutil.Amount

		// suggestions is the set of swaps we expect to be suggested.
		suggestions *Suggestions
	}{
		{
			// Two swaps will cost (78+5000)*2, set exactly 10156
			// budget.
			name:        "budget for 2 swaps, no existing",
			budget:      10156,
			maxMinerFee: 5000,
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1, chan2,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			// Two swaps will cost (78+5000)*2, set 10155 so we can
			// only afford one swap.
			name:        "budget for 1 swaps, no existing",
			budget:      10155,
			maxMinerFee: 5000,
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1,
				},
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID2: ReasonBudgetInsufficient,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			// Set an existing swap which would limit us to a single
			// swap if it were in our period.
			name:        "existing swaps, before budget period",
			budget:      10156,
			maxMinerFee: 5000,
			existingSwaps: map[time.Time]btcutil.Amount{
				testBudgetStart.Add(time.Hour * -1): 200,
			},
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1, chan2,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			// Add an existing swap in our budget period such that
			// we only have budget left for one more swap.
			name:        "existing swaps, in budget period",
			budget:      10156,
			maxMinerFee: 5000,
			existingSwaps: map[time.Time]btcutil.Amount{
				testBudgetStart.Add(time.Hour): 500,
			},
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1,
				},
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID2: ReasonBudgetInsufficient,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:        "existing swaps, budget used",
			budget:      500,
			maxMinerFee: 1000,
			existingSwaps: map[time.Time]btcutil.Amount{
				testBudgetStart.Add(time.Hour): 500,
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonBudgetElapsed,
					chanID2: ReasonBudgetElapsed,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()

			// Create a swap set of existing swaps with our set of
			// existing swap timestamps.
			swaps := make(
				[]*loopdb.LoopOut, 0,
				len(testCase.existingSwaps),
			)

			// Add an event with the timestamp and budget set by
			// our test case.
			for ts, amt := range testCase.existingSwaps {
				event := &loopdb.LoopEvent{
					SwapStateData: loopdb.SwapStateData{
						Cost: loopdb.SwapCost{
							Server: amt,
						},
						State: loopdb.StateSuccess,
					},
					Time: ts,
				}

				swaps = append(swaps, &loopdb.LoopOut{
					Loop: loopdb.Loop{
						Events: []*loopdb.LoopEvent{
							event,
						},
					},
					Contract: autoOutContract,
				})
			}

			cfg.ListLoopOut = func() ([]*loopdb.LoopOut, error) {
				return swaps, nil
			}

			cfg.LoopOutQuote = func(_ context.Context,
				_ *loop.LoopOutQuoteRequest) (*loop.LoopOutQuote,
				error) {

				return quote, nil
			}

			// Set two channels that need swaps.
			lnd.Channels = []lndclient.ChannelInfo{
				channel1,
				channel2,
			}

			params := defaultParameters
			params.ChannelRules = map[lnwire.ShortChannelID]*SwapRule{
				chanID1: chanRule,
				chanID2: chanRule,
			}
			params.AutoFeeBudget = testCase.budget
			params.AutoFeeRefreshPeriod = testBudgetRefresh
			params.AutoloopBudgetLastRefresh = testBudgetStart
			params.MaxAutoInFlight = 2
			params.FeeLimit = NewFeeCategoryLimit(
				defaultSwapFeePPM, defaultRoutingFeePPM,
				defaultPrepayRoutingFeePPM,
				testCase.maxMinerFee, defaultMaximumPrepay,
				defaultSweepFeeRateLimit,
			)

			// Set our custom max miner fee on each expected swap,
			// rather than having to create multiple vars for
			// different rates.
			for i := range testCase.suggestions.OutSwaps {
				testCase.suggestions.OutSwaps[i].MaxMinerFee =
					testCase.maxMinerFee
			}

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.suggestions, nil,
			)
		})
	}
}

// TestInFlightLimit tests the limit we place on the number of in-flight swaps
// that are allowed.
func TestInFlightLimit(t *testing.T) {
	tests := []struct {
		name            string
		maxInFlight     int
		existingSwaps   []*loopdb.LoopOut
		existingInSwaps []*loopdb.LoopIn
		// peerRules will only be set (instead of test default values)
		// is it is non-nil.
		peerRules   map[route.Vertex]*SwapRule
		suggestions *Suggestions
	}{
		{
			name:        "none in flight, extra space",
			maxInFlight: 3,
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1Rec, chan2Rec,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:        "none in flight, exact match",
			maxInFlight: 2,
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1Rec, chan2Rec,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:        "one in flight, one allowed",
			maxInFlight: 2,
			existingSwaps: []*loopdb.LoopOut{
				{
					Contract: autoOutContract,
				},
			},
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1Rec,
				},
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID2: ReasonInFlight,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:        "max in flight",
			maxInFlight: 1,
			existingSwaps: []*loopdb.LoopOut{
				{
					Contract: autoOutContract,
				},
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonInFlight,
					chanID2: ReasonInFlight,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:        "max swaps exceeded",
			maxInFlight: 1,
			existingSwaps: []*loopdb.LoopOut{
				{
					Contract: autoOutContract,
				},
			},
			existingInSwaps: []*loopdb.LoopIn{
				{
					Contract: autoInContract,
				},
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonInFlight,
					chanID2: ReasonInFlight,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:        "peer rules max swaps exceeded",
			maxInFlight: 2,
			existingSwaps: []*loopdb.LoopOut{
				{
					Contract: autoOutContract,
				},
			},
			// Create two peer-level rules, both in need of a swap,
			// but peer 1 needs a larger swap so will be
			// prioritized.
			peerRules: map[route.Vertex]*SwapRule{
				peer1: {
					ThresholdRule: NewThresholdRule(50, 0),
					Type:          swap.TypeOut,
				},
				peer2: {
					ThresholdRule: NewThresholdRule(40, 0),
					Type:          swap.TypeOut,
				},
			},
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1Rec,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: map[route.Vertex]Reason{
					peer2: ReasonInFlight,
				},
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()
			cfg.ListLoopOut = func() ([]*loopdb.LoopOut, error) {
				return testCase.existingSwaps, nil
			}
			cfg.ListLoopIn = func() ([]*loopdb.LoopIn, error) {
				return testCase.existingInSwaps, nil
			}

			lnd.Channels = []lndclient.ChannelInfo{
				channel1, channel2,
			}

			params := defaultParameters
			params.AutoloopBudgetLastRefresh = testBudgetStart

			if testCase.peerRules != nil {
				params.PeerRules = testCase.peerRules
			} else {
				params.ChannelRules =
					map[lnwire.ShortChannelID]*SwapRule{
						chanID1: chanRule,
						chanID2: chanRule,
					}
			}

			params.MaxAutoInFlight = testCase.maxInFlight

			// By default we only have budget for one swap, increase
			// our budget so that we could recommend more than one
			// swap at a time.
			params.AutoFeeBudget = defaultBudget * 2

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.suggestions, nil,
			)
		})
	}
}

type mockServer struct {
	mock.Mock
}

// Restrictions mocks a call to the server to get swap size restrictions.
func (m *mockServer) Restrictions(ctx context.Context, swapType swap.Type) (
	*Restrictions, error) {

	args := m.Called(ctx, swapType)

	return args.Get(0).(*Restrictions), args.Error(1)
}

// TestSizeRestrictions tests the use of client-set size restrictions on swaps.
func TestSizeRestrictions(t *testing.T) {
	var (
		serverRestrictions = Restrictions{
			Minimum: 6000,
			Maximum: 10000,
		}

		prepay, routing = testPPMFees(defaultFeePPM, testQuote, 7000)
		outSwap         = loop.OutRequest{
			Amount:              7000,
			OutgoingChanSet:     loopdb.ChannelSet{chanID1.ToUint64()},
			MaxPrepayRoutingFee: prepay,
			MaxSwapRoutingFee:   routing,
			MaxMinerFee:         scaleMinerFee(testQuote.MinerFee),
			MaxSwapFee:          testQuote.SwapFee,
			MaxPrepayAmount:     testQuote.PrepayAmount,
			SweepConfTarget:     defaultConfTarget,
			Initiator:           autoloopSwapInitiator,
		}
	)

	tests := []struct {
		name string

		// clientRestrictions holds the restrictions that the client
		// has configured.
		clientRestrictions Restrictions

		prepareMock func(m *mockServer)

		// suggestions is the set of suggestions we expect.
		suggestions *Suggestions

		// expectedError is the error we expect.
		expectedError error
	}{
		{
			name: "minimum more than server, swap happens",
			clientRestrictions: Restrictions{
				Minimum: 7000,
			},
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					chan1Rec,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "minimum more than server, no swap",
			clientRestrictions: Restrictions{
				Minimum: 8000,
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonLiquidityOk,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "maximum less than server, swap happens",
			clientRestrictions: Restrictions{
				Maximum: 7000,
			},
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					outSwap,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			// Originally, our client params are ok. But then the
			// server increases its minimum, making the client
			// params stale.
			name: "client params stale over time",
			clientRestrictions: Restrictions{
				Minimum: 6500,
				Maximum: 9000,
			},
			prepareMock: func(m *mockServer) {
				restrictions := serverRestrictions

				m.On(
					"Restrictions", mock.Anything,
					swap.TypeOut,
				).Return(
					&restrictions, nil,
				).Once()

				m.On(
					"Restrictions", mock.Anything,
					swap.TypeOut,
				).Return(
					&Restrictions{
						Minimum: 5000,
						Maximum: 6000,
					}, nil,
				).Once()
			},
			suggestions:   nil,
			expectedError: ErrMaxExceedsServer,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()

			lnd.Channels = []lndclient.ChannelInfo{
				channel1,
			}

			params := defaultParameters
			params.AutoloopBudgetLastRefresh = testBudgetStart
			params.ClientRestrictions = testCase.clientRestrictions
			params.ChannelRules = map[lnwire.ShortChannelID]*SwapRule{
				chanID1: chanRule,
			}

			// Use a mock that has our expected calls for the test
			// case set to provide server restrictions.
			mockServer := &mockServer{}

			// If the test wants us to prime the mock, use its
			// function, otherwise just return our default
			// restrictions.
			if testCase.prepareMock != nil {
				testCase.prepareMock(mockServer)
			} else {
				restrictions := serverRestrictions

				mockServer.On(
					"Restrictions", mock.Anything,
					mock.Anything,
				).Return(&restrictions, nil)
			}

			cfg.Restrictions = mockServer.Restrictions

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.suggestions, testCase.expectedError,
			)

			mockServer.AssertExpectations(t)
		})
	}
}

// TestFeePercentage tests use of a flat fee percentage to limit the fees we
// pay for swaps. Our test is setup to require a 7500 sat swap, and we test
// this amount against various fee percentages and server quotes.
func TestFeePercentage(t *testing.T) {
	var (
		okPPM   uint64 = 30000
		okQuote        = &loop.LoopOutQuote{
			SwapFee:      15,
			PrepayAmount: 30,
			MinerFee:     1,
		}

		rec = loop.OutRequest{
			Amount:          7500,
			OutgoingChanSet: loopdb.ChannelSet{chanID1.ToUint64()},
			MaxMinerFee:     scaleMinerFee(okQuote.MinerFee),
			MaxSwapFee:      okQuote.SwapFee,
			MaxPrepayAmount: okQuote.PrepayAmount,
			SweepConfTarget: defaultConfTarget,
			Initiator:       autoloopSwapInitiator,
		}
	)

	rec.MaxPrepayRoutingFee, rec.MaxSwapRoutingFee = testPPMFees(
		okPPM, okQuote, 7500,
	)

	tests := []struct {
		name        string
		feePPM      uint64
		quote       *loop.LoopOutQuote
		suggestions *Suggestions
	}{
		{
			// With our limit set to 3% of swap amount 7500, we
			// have a total budget of 225 sat.
			name:   "fees ok",
			feePPM: okPPM,
			quote:  okQuote,
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					rec,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:   "swap fee too high",
			feePPM: 20000,
			quote: &loop.LoopOutQuote{
				SwapFee:      300,
				PrepayAmount: 30,
				MinerFee:     1,
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonSwapFee,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:   "miner fee too high",
			feePPM: 20000,
			quote: &loop.LoopOutQuote{
				SwapFee:      80,
				PrepayAmount: 30,
				MinerFee:     300,
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonMinerFee,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:   "miner and swap too high",
			feePPM: 20000,
			quote: &loop.LoopOutQuote{
				SwapFee:      60,
				PrepayAmount: 30,
				MinerFee:     1,
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonFeePPMInsufficient,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name:   "prepay too high",
			feePPM: 30000,
			quote: &loop.LoopOutQuote{
				SwapFee:      75,
				PrepayAmount: 300,
				MinerFee:     1,
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonPrepay,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()

			cfg.LoopOutQuote = func(_ context.Context,
				_ *loop.LoopOutQuoteRequest) (*loop.LoopOutQuote,
				error) {

				return testCase.quote, nil
			}

			lnd.Channels = []lndclient.ChannelInfo{
				channel1,
			}

			params := defaultParameters
			params.AutoloopBudgetLastRefresh = testBudgetStart
			params.FeeLimit = NewFeePortion(testCase.feePPM)
			params.ChannelRules = map[lnwire.ShortChannelID]*SwapRule{
				chanID1: chanRule,
			}

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.suggestions, nil,
			)
		})
	}
}

// TestBudgetWithLoopin tests that our autoloop budget accounts for loop in
// swaps that have been automatically dispatched. It tests out swaps that have
// already completed and those that are pending, inside and outside of our
// budget period to ensure that we account for all relevant swaps.
func TestBudgetWithLoopin(t *testing.T) {
	var (
		budget btcutil.Amount = 10000

		outsideBudget = testBudgetStart.Add(-5)
		insideBudget  = testBudgetStart.Add(5)

		contractOutsideBudget = &loopdb.LoopInContract{
			SwapContract: loopdb.SwapContract{
				InitiationTime: outsideBudget,
				MaxSwapFee:     budget,
				Label: labels.AutoloopLabel(
					swap.TypeIn,
				),
			},
		}

		// Set our spend equal to our budget so we don't need to
		// calculate exact costs.
		eventOutsideBudget = &loopdb.LoopEvent{
			SwapStateData: loopdb.SwapStateData{
				Cost: loopdb.SwapCost{
					Server: budget,
				},
				State: loopdb.StateSuccess,
			},
			Time: outsideBudget,
		}

		successWithinBudget = &loopdb.LoopEvent{
			SwapStateData: loopdb.SwapStateData{
				Cost: loopdb.SwapCost{
					Server: budget,
				},
				State: loopdb.StateSuccess,
			},
			Time: insideBudget,
		}

		okQuote = &loop.LoopOutQuote{
			SwapFee:      15,
			PrepayAmount: 30,
			MinerFee:     1,
		}

		rec = loop.OutRequest{
			Amount:          7500,
			OutgoingChanSet: loopdb.ChannelSet{chanID1.ToUint64()},
			MaxMinerFee:     scaleMinerFee(okQuote.MinerFee),
			MaxSwapFee:      okQuote.SwapFee,
			MaxPrepayAmount: okQuote.PrepayAmount,
			SweepConfTarget: defaultConfTarget,
			Initiator:       autoloopSwapInitiator,
		}

		testPPM uint64 = 100000
	)

	rec.MaxPrepayRoutingFee, rec.MaxSwapRoutingFee = testPPMFees(
		testPPM, okQuote, 7500,
	)

	tests := []struct {
		name string

		// loopIns is the set of loop in swaps that the client has
		// performed.
		loopIns []*loopdb.LoopIn

		// suggestions is the set of swaps that we expect to be
		// suggested given our current traffic.
		suggestions *Suggestions
	}{
		{
			name: "completed swap outside of budget",
			loopIns: []*loopdb.LoopIn{
				{
					Loop: loopdb.Loop{
						Events: []*loopdb.LoopEvent{
							eventOutsideBudget,
						},
					},
					Contract: contractOutsideBudget,
				},
			},
			suggestions: &Suggestions{
				OutSwaps: []loop.OutRequest{
					rec,
				},
				DisqualifiedChans: noneDisqualified,
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "completed within budget",
			loopIns: []*loopdb.LoopIn{
				{
					Loop: loopdb.Loop{
						Events: []*loopdb.LoopEvent{
							successWithinBudget,
						},
					},
					Contract: contractOutsideBudget,
				},
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonBudgetElapsed,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
		{
			name: "pending created before budget",
			loopIns: []*loopdb.LoopIn{
				{
					Contract: contractOutsideBudget,
				},
			},
			suggestions: &Suggestions{
				DisqualifiedChans: map[lnwire.ShortChannelID]Reason{
					chanID1: ReasonBudgetElapsed,
				},
				DisqualifiedPeers: noPeersDisqualified,
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()

			// Set our channel and rules so that we will need to
			// swap 7500 sats and our fee limit is 10% of that
			// amount (750 sats).
			lnd.Channels = []lndclient.ChannelInfo{
				channel1,
			}

			cfg.ListLoopIn = func() ([]*loopdb.LoopIn, error) {
				return testCase.loopIns, nil
			}

			cfg.LoopOutQuote = func(_ context.Context,
				_ *loop.LoopOutQuoteRequest) (*loop.LoopOutQuote,
				error) {

				return okQuote, nil
			}

			params := defaultParameters
			params.AutoFeeBudget = budget
			params.AutoFeeRefreshPeriod = testBudgetRefresh
			params.AutoloopBudgetLastRefresh = testBudgetStart

			params.FeeLimit = NewFeePortion(testPPM)
			params.ChannelRules = map[lnwire.ShortChannelID]*SwapRule{
				chanID1: chanRule,
			}

			// Allow more than one in flight swap, to ensure that
			// we restrict based on budget, not in-flight.
			params.MaxAutoInFlight = 2

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.suggestions, nil,
			)
		})
	}
}

// testSuggestSwapsSetup contains the elements that are used to create a
// suggest swaps test.
type testSuggestSwapsSetup struct {
	cfg    *Config
	lnd    *test.LndMockServices
	params Parameters
}

// newSuggestSwapsSetup creates a suggest swaps setup struct.
func newSuggestSwapsSetup(cfg *Config, lnd *test.LndMockServices,
	params Parameters) *testSuggestSwapsSetup {

	return &testSuggestSwapsSetup{
		cfg:    cfg,
		lnd:    lnd,
		params: params,
	}
}

// testSuggestSwaps tests getting swap suggestions. It takes a setup struct
// which contains custom setup for the test. If this struct is nil, it will
// use the default parameters and setup two channels (channel1 + channel2) with
// chanRule set for each.
func testSuggestSwaps(t *testing.T, setup *testSuggestSwapsSetup,
	expected *Suggestions, expectedErr error) {

	t.Parallel()

	// If our setup struct is nil, we replace it with our default test
	// values.
	if setup == nil {
		cfg, lnd := newTestConfig()

		lnd.Channels = []lndclient.ChannelInfo{
			channel1, channel2,
		}

		params := defaultParameters
		params.AutoloopBudgetLastRefresh = testBudgetStart
		params.ChannelRules = map[lnwire.ShortChannelID]*SwapRule{
			chanID1: chanRule,
			chanID2: chanRule,
		}

		setup = &testSuggestSwapsSetup{
			cfg:    cfg,
			lnd:    lnd,
			params: params,
		}
	}

	// Create a new manager, get our current set of parameters and update
	// them to use the rules set by the test.
	manager := NewManager(setup.cfg)

	err := manager.setParameters(context.Background(), setup.params)
	require.NoError(t, err)

	actual, err := manager.SuggestSwaps(context.Background(), false)
	require.Equal(t, expectedErr, err)
	require.Equal(t, expected, actual)
}

// TestCurrentTraffic tests recording of our current set of ongoing swaps.
func TestCurrentTraffic(t *testing.T) {
	var (
		backoff        = time.Hour * 5
		withinBackoff  = testTime.Add(time.Hour * -1)
		outsideBackoff = testTime.Add(backoff * -2)

		success = []*loopdb.LoopEvent{
			{
				SwapStateData: loopdb.SwapStateData{
					State: loopdb.StateSuccess,
				},
			},
		}

		failedInBackoff = []*loopdb.LoopEvent{
			{
				SwapStateData: loopdb.SwapStateData{
					State: loopdb.StateFailOffchainPayments,
				},
				Time: withinBackoff,
			},
		}

		failedOutsideBackoff = []*loopdb.LoopEvent{
			{
				SwapStateData: loopdb.SwapStateData{
					State: loopdb.StateFailOffchainPayments,
				},
				Time: outsideBackoff,
			},
		}

		failedTimeoutInBackoff = []*loopdb.LoopEvent{
			{
				SwapStateData: loopdb.SwapStateData{
					State: loopdb.StateFailTimeout,
				},
				Time: withinBackoff,
			},
		}

		failedTimeoutOutsideBackoff = []*loopdb.LoopEvent{
			{
				SwapStateData: loopdb.SwapStateData{
					State: loopdb.StateFailTimeout,
				},
				Time: outsideBackoff,
			},
		}
	)

	tests := []struct {
		name     string
		loopOut  []*loopdb.LoopOut
		loopIn   []*loopdb.LoopIn
		expected *swapTraffic
	}{
		{
			name: "completed swaps ignored",
			loopOut: []*loopdb.LoopOut{
				{
					Loop: loopdb.Loop{
						Events: success,
					},
					Contract: &loopdb.LoopOutContract{},
				},
			},
			loopIn: []*loopdb.LoopIn{
				{
					Loop: loopdb.Loop{
						Events: success,
					},
					Contract: &loopdb.LoopInContract{},
				},
			},
			expected: newSwapTraffic(),
		},
		{
			// No events indicates that the swap is still pending.
			name: "pending swaps included",
			loopOut: []*loopdb.LoopOut{
				{
					Contract: &loopdb.LoopOutContract{
						OutgoingChanSet: []uint64{
							chanID1.ToUint64(),
						},
					},
				},
			},
			loopIn: []*loopdb.LoopIn{
				{
					Contract: &loopdb.LoopInContract{
						LastHop: &peer2,
					},
				},
			},
			expected: &swapTraffic{
				ongoingLoopOut: map[lnwire.ShortChannelID]bool{
					chanID1: true,
				},
				ongoingLoopIn: map[route.Vertex]bool{
					peer2: true,
				},
				// Make empty maps so that we can assert equal.
				failedLoopOut: make(
					map[lnwire.ShortChannelID]time.Time,
				),
				failedLoopIn: make(map[route.Vertex]time.Time),
			},
		},
		{
			name: "failure backoff included",
			loopOut: []*loopdb.LoopOut{
				{
					Contract: &loopdb.LoopOutContract{
						OutgoingChanSet: []uint64{
							chanID1.ToUint64(),
						},
					},
					Loop: loopdb.Loop{
						Events: failedInBackoff,
					},
				},
				{
					Contract: &loopdb.LoopOutContract{
						OutgoingChanSet: []uint64{
							chanID2.ToUint64(),
						},
					},
					Loop: loopdb.Loop{
						Events: failedOutsideBackoff,
					},
				},
			},
			loopIn: []*loopdb.LoopIn{
				{
					Contract: &loopdb.LoopInContract{
						LastHop: &peer1,
					},
					Loop: loopdb.Loop{
						Events: failedTimeoutInBackoff,
					},
				},
				{
					Contract: &loopdb.LoopInContract{
						LastHop: &peer2,
					},
					Loop: loopdb.Loop{
						Events: failedTimeoutOutsideBackoff,
					},
				},
			},
			expected: &swapTraffic{
				ongoingLoopOut: make(
					map[lnwire.ShortChannelID]bool,
				),
				ongoingLoopIn: make(map[route.Vertex]bool),
				failedLoopOut: map[lnwire.ShortChannelID]time.Time{
					chanID1: withinBackoff,
				},
				failedLoopIn: map[route.Vertex]time.Time{
					peer1: withinBackoff,
				},
			},
		},
	}

	for _, testCase := range tests {
		cfg, _ := newTestConfig()
		m := NewManager(cfg)

		params := m.GetParameters()
		params.FailureBackOff = backoff
		require.NoError(t, m.setParameters(context.Background(), params))

		actual := m.currentSwapTraffic(testCase.loopOut, testCase.loopIn)
		require.Equal(t, testCase.expected, actual)
	}
}
