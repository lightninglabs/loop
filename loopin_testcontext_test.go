package loop

import (
	"testing"
	"time"

	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/test"
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
}

func newLoopInTestContext(t *testing.T) *loopInTestContext {
	lnd := test.NewMockLnd()
	server := newServerMock()
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
	}
}

func (c *loopInTestContext) assertState(expectedState loopdb.SwapState) {
	state := <-c.statusChan
	if state.State != expectedState {
		c.t.Fatalf("expected state %v but got %v", expectedState,
			state.State)
	}
}
