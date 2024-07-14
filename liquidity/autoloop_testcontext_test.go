package liquidity

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	defaultEventuallyTimeout  = time.Second * 45
	defaultEventuallyInterval = time.Millisecond * 100
)

type autoloopTestCtx struct {
	t         *testing.T
	manager   *Manager
	lnd       *test.LndMockServices
	testClock *clock.TestClock

	// quoteRequest is a channel that requests for quotes are pushed into.
	quoteRequest chan *loop.LoopOutQuoteRequest

	// quotes is a channel that we get loop out quote requests on.
	quotes chan *loop.LoopOutQuote

	// quoteRequestIn is a channel that requests for loop in quotes are
	// pushed into.
	quoteRequestIn chan *loop.LoopInQuoteRequest

	// quotesIn is a channel that we get loop in quote responses on.
	quotesIn chan *loop.LoopInQuote

	// loopOutRestrictions is a channel that we get the server's
	// restrictions on.
	loopOutRestrictions chan *Restrictions

	// loopInRestrictions is a channel that we get the server's
	// loop in restrictions on.
	loopInRestrictions chan *Restrictions

	// loopOuts is a channel that we get existing loop out swaps on.
	loopOuts chan []*loopdb.LoopOut

	// loopOutSingle is the single loop out returned from fetching a single
	// swap from store.
	loopOutSingle *loopdb.LoopOut

	// loopIns is a channel that we get existing loop in swaps on.
	loopIns chan []*loopdb.LoopIn

	// loopInSingle is the single loop in returned from fetching a single
	// swap from store.
	loopInSingle *loopdb.LoopIn

	// restrictions is a channel that we get swap restrictions on.
	restrictions chan *Restrictions

	// outRequest is a channel that requests to dispatch loop outs are
	// pushed into.
	outRequest chan *loop.OutRequest

	// loopOut is a channel that we return loop out responses on.
	loopOut chan *loop.LoopOutSwapInfo

	// inRequest is a channel that requests to dispatch loop in swaps are
	// pushed into.
	inRequest chan *loop.LoopInRequest

	// loopIn is a channel that we return loop in responses on.
	loopIn chan *loop.LoopInSwapInfo

	// errChan is a channel that we send run errors into.
	errChan chan error

	// cancelCtx cancels the context that our liquidity manager is run with.
	// This can be used to cleanly shutdown the test. Note that this will be
	// nil until the test context has been started.
	cancelCtx func()
}

// newAutoloopTestCtx creates a test context with custom liquidity manager
// parameters and lnd channels.
func newAutoloopTestCtx(t *testing.T, parameters Parameters,
	channels []lndclient.ChannelInfo,
	server *Restrictions) *autoloopTestCtx {

	// Create a mock lnd and set our expected fee rate for sweeps to our
	// sweep fee rate limit value.
	lnd := test.NewMockLnd()

	categories, ok := parameters.FeeLimit.(*FeeCategoryLimit)
	if ok {
		lnd.SetFeeEstimate(
			parameters.SweepConfTarget, categories.SweepFeeRateLimit,
		)
	}

	testCtx := &autoloopTestCtx{
		t:         t,
		testClock: clock.NewTestClock(testTime),
		lnd:       lnd,

		quoteRequest:        make(chan *loop.LoopOutQuoteRequest),
		quotes:              make(chan *loop.LoopOutQuote),
		quoteRequestIn:      make(chan *loop.LoopInQuoteRequest),
		quotesIn:            make(chan *loop.LoopInQuote),
		loopOutRestrictions: make(chan *Restrictions),
		loopInRestrictions:  make(chan *Restrictions),
		loopOuts:            make(chan []*loopdb.LoopOut),
		loopIns:             make(chan []*loopdb.LoopIn),
		restrictions:        make(chan *Restrictions),
		outRequest:          make(chan *loop.OutRequest),
		loopOut:             make(chan *loop.LoopOutSwapInfo),
		inRequest:           make(chan *loop.LoopInRequest),
		loopIn:              make(chan *loop.LoopInSwapInfo),
		errChan:             make(chan error, 1),
	}

	// Set lnd's channels to equal the set of channels we want for our
	// test.
	testCtx.lnd.Channels = channels

	cfg := &Config{
		AutoloopTicker: ticker.NewForce(DefaultAutoloopTicker),
		Restrictions: func(_ context.Context, swapType swap.Type, initiator string) (*Restrictions,
			error) {

			if swapType == swap.TypeOut {
				return <-testCtx.loopOutRestrictions, nil
			}

			return <-testCtx.loopInRestrictions, nil
		},
		ListLoopOut: func(context.Context) ([]*loopdb.LoopOut, error) {
			return <-testCtx.loopOuts, nil
		},
		GetLoopOut: func(ctx context.Context,
			hash lntypes.Hash) (*loopdb.LoopOut, error) {

			return testCtx.loopOutSingle, nil
		},
		ListLoopIn: func(context.Context) ([]*loopdb.LoopIn, error) {
			return <-testCtx.loopIns, nil
		},
		LoopOutQuote: func(_ context.Context,
			req *loop.LoopOutQuoteRequest) (*loop.LoopOutQuote,
			error) {

			testCtx.quoteRequest <- req

			return <-testCtx.quotes, nil
		},
		LoopOut: func(_ context.Context,
			req *loop.OutRequest) (*loop.LoopOutSwapInfo,
			error) {

			testCtx.outRequest <- req

			return <-testCtx.loopOut, nil
		},
		LoopInQuote: func(_ context.Context,
			req *loop.LoopInQuoteRequest) (*loop.LoopInQuote, error) {

			testCtx.quoteRequestIn <- req

			return <-testCtx.quotesIn, nil
		},
		LoopIn: func(_ context.Context,
			req *loop.LoopInRequest) (*loop.LoopInSwapInfo, error) {

			testCtx.inRequest <- req

			return <-testCtx.loopIn, nil
		},
		MinimumConfirmations: loop.DefaultSweepConfTarget,
		Lnd:                  &testCtx.lnd.LndServices,
		Clock:                testCtx.testClock,
		PutLiquidityParams: func(_ context.Context, _ []byte) error {
			return nil
		},
		FetchLiquidityParams: func(context.Context) ([]byte, error) {
			return nil, nil
		},
	}

	// SetParameters needs to make a call to our mocked restrictions call,
	// which will block, so we push our test values in a goroutine.
	done := make(chan struct{})
	go func() {
		testCtx.loopOutRestrictions <- server
		close(done)
	}()

	// Create a manager with our test config and set our starting set of
	// parameters.
	testCtx.manager = NewManager(cfg)
	err := testCtx.manager.setParameters(context.Background(), parameters)
	assert.NoError(t, err)
	// Override the payments check interval for the tests in order to not
	// timeout.
	testCtx.manager.params.CustomPaymentCheckInterval =
		150 * time.Millisecond
	<-done
	return testCtx
}

// start starts our liquidity manager's run loop in a goroutine. Tests should
// be run with test.Guard() to ensure that this does not leak.
func (c *autoloopTestCtx) start() {
	ctx := context.Background()
	ctx, c.cancelCtx = context.WithCancel(ctx)

	go func() {
		c.errChan <- c.manager.Run(ctx)
	}()
}

// stop shuts down our test context and asserts that we have exited with a
// context-cancelled error.
func (c *autoloopTestCtx) stop() {
	c.cancelCtx()
	assert.Equal(c.t, context.Canceled, <-c.errChan)
}

// quoteRequestResp pairs an expected swap quote request with the response we
// would like to provide the liquidity manager with.
type quoteRequestResp struct {
	request *loop.LoopOutQuoteRequest
	quote   *loop.LoopOutQuote
}

// loopOutRequestResp pairs an expected loop out request with the response we
// would like the server to respond with.
type loopOutRequestResp struct {
	request  *loop.OutRequest
	response *loop.LoopOutSwapInfo
}

// quoteInRequestResp pairs an expected loop in quote request with the response
// we would like to provide the manager with.
type quoteInRequestResp struct {
	request *loop.LoopInQuoteRequest
	quote   *loop.LoopInQuote
}

// loopInRequestResp pairs and expected loop in request with the response we
// would like the mocked server to respond with.
type loopInRequestResp struct {
	request  *loop.LoopInRequest
	response *loop.LoopInSwapInfo
}

// autoloopStep contains all of the information to required to step
// through an autoloop tick.
type autoloopStep struct {
	minAmt            btcutil.Amount
	maxAmt            btcutil.Amount
	existingOut       []*loopdb.LoopOut
	existingOutSingle *loopdb.LoopOut
	existingIn        []*loopdb.LoopIn
	existingInSingle  *loopdb.LoopIn
	quotesOut         []quoteRequestResp
	quotesIn          []quoteInRequestResp
	expectedOut       []loopOutRequestResp
	expectedIn        []loopInRequestResp
	keepDestAddr      bool
}

type easyAutoloopStep struct {
	minAmt      btcutil.Amount
	maxAmt      btcutil.Amount
	existingOut []*loopdb.LoopOut
	existingIn  []*loopdb.LoopIn
	quotesOut   []quoteRequestResp
	expectedOut []loopOutRequestResp
}

// autoloop walks our test context through the process of triggering our
// autoloop functionality, providing mocked values as required. The set of
// quotes provided indicates that we expect swap suggestions to be made (since
// we will query for a quote for each suggested swap). The set of expected
// swaps indicates whether we expect any of these swap suggestions to actually
// be dispatched by the autolooper.
func (c *autoloopTestCtx) autoloop(step *autoloopStep) {
	// Tick our autoloop ticker to force assessing whether we want to loop.
	c.manager.cfg.AutoloopTicker.Force <- testTime

	// Send a mocked response from the server with the swap size limits.
	c.loopOutRestrictions <- NewRestrictions(step.minAmt, step.maxAmt)
	c.loopInRestrictions <- NewRestrictions(step.minAmt, step.maxAmt)

	// Provide the liquidity manager with our desired existing set of swaps.
	c.loopOuts <- step.existingOut
	c.loopIns <- step.existingIn

	c.loopOutSingle = step.existingOutSingle
	c.loopInSingle = step.existingInSingle

	// Assert that we query the server for a quote for each of our
	// recommended swaps. Note that this differs from our set of expected
	// swaps because we may get quotes for suggested swaps but then just
	// log them. The order in c.quoteRequestIn is not deterministic,
	// it depends on the order of map traversal (map peerChannels in
	// method Manager.SuggestSwaps). So receive from the channel an item
	// and then find a corresponding expected item, using amount as a key.
	amt2expected := make(map[btcutil.Amount]quoteInRequestResp)
	for _, expected := range step.quotesIn {
		// Make sure all amounts are unique.
		require.NotContains(c.t, amt2expected, expected.request.Amount)

		amt2expected[expected.request.Amount] = expected
	}

	for i := 0; i < len(step.quotesIn); i++ {
		request := <-c.quoteRequestIn

		// Get the expected item, using amount as a key.
		expected, has := amt2expected[request.Amount]
		require.True(c.t, has)
		delete(amt2expected, request.Amount)

		assert.Equal(
			c.t, expected.request.Amount, request.Amount,
		)

		assert.Equal(
			c.t, expected.request.HtlcConfTarget,
			request.HtlcConfTarget,
		)

		c.quotesIn <- expected.quote
	}

	for _, expected := range step.quotesOut {
		request := <-c.quoteRequest
		assert.Equal(
			c.t, expected.request.Amount, request.Amount,
		)
		assert.Equal(
			c.t, expected.request.SweepConfTarget,
			request.SweepConfTarget,
		)
		c.quotes <- expected.quote
	}

	require.True(c.t, c.matchLoopOuts(step.expectedOut, step.keepDestAddr))
	require.True(c.t, c.matchLoopIns(step.expectedIn))

	require.Eventuallyf(c.t, func() bool {
		return c.manager.numActiveStickyLoops() == 0
	}, defaultEventuallyTimeout, defaultEventuallyInterval, "failed to"+
		" wait for sticky loop counter")

	// Since we're checking if any false-positive swaps were dispatched we
	// need to give some time to autoloop to possibly dispatch them.
	select {
	case <-c.outRequest:
		c.t.Fatal("expected no more loopout requests")

	case <-c.inRequest:
		c.t.Fatal("expected no more loopin requests")

	case <-c.quoteRequestIn:
		c.t.Fatal("expected no more loopout quote requests")

	case <-c.quoteRequest:
		c.t.Fatal("expected no more loopin quote requests")

	case <-time.After(500 * time.Millisecond):
	}
}

// easyautoloop walks our test context through the process of triggering our
// easy autoloop functionality, providing mocked values as required. The number
// of values needed to mock easy autoloop are less than standard autoloop as the
// goal of easy autoloop is to simplify its usage.
func (c *autoloopTestCtx) easyautoloop(step *easyAutoloopStep, noop bool) {
	// Tick our autoloop ticker to force assessing whether we want to loop.
	c.manager.cfg.AutoloopTicker.Force <- testTime

	// Provide the liquidity manager with our desired existing set of swaps.
	c.loopOuts <- step.existingOut
	c.loopIns <- step.existingIn

	// If easy autoloop is not meant to be triggered we skip sending the
	// mock response for restrictions, as this is never called.
	if !noop {
		// Send a mocked response from the server with the swap size limits.
		c.loopOutRestrictions <- NewRestrictions(step.minAmt, step.maxAmt)
	}

	for _, expected := range step.quotesOut {
		request := <-c.quoteRequest
		require.Equal(
			c.t, expected.request.Amount, request.Amount,
		)

		c.quotes <- expected.quote
	}

	for _, expected := range step.expectedOut {
		actual := <-c.outRequest

		require.Equal(c.t, expected.request.Amount, actual.Amount)
		require.Equal(
			c.t, expected.request.OutgoingChanSet,
			actual.OutgoingChanSet,
		)
		if expected.request.DestAddr != nil {
			require.Equal(
				c.t, expected.request.DestAddr, actual.DestAddr,
			)
		}
	}

	// Since we're checking if any false-positive swaps were dispatched we
	// need to give some time to autoloop to possibly dispatch them.
	select {
	case <-c.outRequest:
		c.t.Fatal("expected no more loopout requests")

	case <-c.inRequest:
		c.t.Fatal("expected no more loopin requests")

	case <-c.quoteRequestIn:
		c.t.Fatal("expected no more loopout quote requests")

	case <-c.quoteRequest:
		c.t.Fatal("expected no more loopin quote requests")

	case <-time.After(500 * time.Millisecond):
	}
}

// matchLoopOuts checks that the actual loop out requests we got match the
// expected ones. The argument keepDestAddr is used to indicate whether we keep
// the actual loops destination address for the comparison. This is useful
// because we don't want to compare the destination address generated by the
// wallet mock. We want to compare the destination address when testing the
// autoloop DestAddr parameter for loop outs.
func (c *autoloopTestCtx) matchLoopOuts(swaps []loopOutRequestResp,
	keepDestAddr bool) bool {

	swapsCopy := make([]loopOutRequestResp, len(swaps))
	copy(swapsCopy, swaps)

	length := len(swapsCopy)

	for i := 0; i < length; i++ {
		actual := <-c.outRequest

		if !keepDestAddr {
			actual.DestAddr = nil
		}

	inner:
		for index, swap := range swapsCopy {
			equal := reflect.DeepEqual(swap.request, actual)

			if equal {
				c.loopOut <- swap.response

				swapsCopy = append(
					swapsCopy[:index],
					swapsCopy[index+1:]...,
				)

				break inner
			}
		}
	}

	return len(swapsCopy) == 0
}

// matchLoopIns checks that the actual loop in requests we got match the
// expected ones.
func (c *autoloopTestCtx) matchLoopIns(
	swaps []loopInRequestResp) bool {

	swapsCopy := make([]loopInRequestResp, len(swaps))
	copy(swapsCopy, swaps)

	for i := 0; i < len(swapsCopy); i++ {
		actual := <-c.inRequest

	inner:
		for i, swap := range swapsCopy {
			equal := reflect.DeepEqual(swap.request, actual)

			if equal {
				c.loopIn <- swap.response

				swapsCopy = append(
					swapsCopy[:i], swapsCopy[i+1:]...,
				)

				break inner
			}
		}
	}

	return len(swapsCopy) == 0
}
