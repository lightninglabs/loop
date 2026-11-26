package loopin

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/loop"
	"github.com/stretchr/testify/require"
)

// TestDeliverLoopInRequestAbandonedCaller checks that the manager's run loop
// can still hand over its response after the caller of DeliverLoopInRequest
// stopped waiting. The run loop only stops waiting for the response to be
// taken when the manager shuts down, so a response that cannot be delivered
// would block every later swap request.
func TestDeliverLoopInRequestAbandonedCaller(t *testing.T) {
	m := &Manager{
		newLoopInChan: make(chan *newSwapRequest),
		exitChan:      make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := m.DeliverLoopInRequest(
			ctx, &loop.StaticAddressLoopInRequest{},
		)
		done <- err
	}()

	// Take the request like the run loop does.
	var request *newSwapRequest
	select {
	case request = <-m.newLoopInChan:
	case <-time.After(time.Second):
		require.FailNow(t, "request was not delivered")
	}

	// The caller goes away while the swap is still being initiated.
	cancel()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "context canceled")
	case <-time.After(time.Second):
		require.FailNow(t, "caller did not return")
	}

	// The run loop must be able to deliver its response regardless.
	select {
	case request.respChan <- &newSwapResponse{}:
	case <-time.After(time.Second):
		require.FailNow(t, "response to an abandoned request blocks "+
			"the manager run loop")
	}
}
