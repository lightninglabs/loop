package loop

import (
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/test"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

type loopInTestContext struct {
	t              *testing.T
	lnd            *test.LndMockServices
	server         *serverMock
	store          *storeMock
	sweeper        *sweep.Sweeper
	cfg            *executeConfig
	statusChan     chan SwapInfo
	blockEpochChan chan interface{}
	wg             *sync.WaitGroup

	swapInvoiceSubscription *test.SingleInvoiceSubscription
}

func newLoopInTestContext(t *testing.T) *loopInTestContext {
	lnd := test.NewMockLnd()
	server := newServerMock(lnd)
	store := newStoreMock(t)
	sweeper := sweep.Sweeper{Lnd: &lnd.LndServices}

	blockEpochChan := make(chan interface{})
	statusChan := make(chan SwapInfo)

	expiryChan := make(chan time.Time)
	timerFactory := func(expiry time.Duration) <-chan time.Time {
		return expiryChan
	}

	cfg := executeConfig{
		statusChan:     statusChan,
		sweeper:        &sweeper,
		blockEpochChan: blockEpochChan,
		timerFactory:   timerFactory,
		cancelSwap:     server.CancelLoopOutSwap,
	}

	return &loopInTestContext{
		t:              t,
		lnd:            lnd,
		server:         server,
		store:          store,
		sweeper:        &sweeper,
		cfg:            &cfg,
		statusChan:     statusChan,
		blockEpochChan: blockEpochChan,
		wg:             &sync.WaitGroup{},
	}
}

func (c *loopInTestContext) assertState(expectedState loopdb.SwapState) {
	state := <-c.statusChan
	require.Equal(c.t, expectedState, state.State)
}

// assertSubscribeInvoice asserts that the client subscribes to invoice updates
// for our swap invoice.
func (c *loopInTestContext) assertSubscribeInvoice(hash lntypes.Hash) {
	c.swapInvoiceSubscription = <-c.lnd.SingleInvoiceSubcribeChannel
	require.Equal(c.t, hash, c.swapInvoiceSubscription.Hash)
}

// updateInvoiceState mocks an update to our swap invoice state.
func (c *loopInTestContext) updateInvoiceState(amount btcutil.Amount,
	state invpkg.ContractState) {

	c.swapInvoiceSubscription.Update <- lndclient.InvoiceUpdate{
		AmtPaid: amount,
		State:   state,
	}

	// If we're in a final state, close our update channels as lndclient
	// would.
	if state == invpkg.ContractCanceled ||
		state == invpkg.ContractSettled {

		close(c.swapInvoiceSubscription.Update)
		close(c.swapInvoiceSubscription.Err)
	}
}
