package openchannel

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

// TestDeliverOpenChannelRequestAbandonedCaller checks that the manager's run
// loop can still hand over its response after the caller of
// DeliverOpenChannelRequest stopped waiting. A response that cannot be
// delivered would block every later open channel request.
func TestDeliverOpenChannelRequestAbandonedCaller(t *testing.T) {
	m := &Manager{
		newOpenChannelRequestChan: make(chan newOpenChannelRequest),
		exitChan:                  make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := m.DeliverOpenChannelRequest(
			ctx, &lnrpc.OpenChannelRequest{},
		)
		done <- err
	}()

	// Take the request like the run loop does.
	var request newOpenChannelRequest
	select {
	case request = <-m.newOpenChannelRequestChan:
	case <-time.After(time.Second):
		require.FailNow(t, "request was not delivered")
	}

	// The caller goes away while the channel is still being opened.
	cancel()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "context canceled")
	case <-time.After(time.Second):
		require.FailNow(t, "caller did not return")
	}

	// The run loop must be able to deliver its response regardless.
	select {
	case request.respChan <- &newOpenChannelResponse{}:
	case <-time.After(time.Second):
		require.FailNow(t, "response to an abandoned request blocks "+
			"the manager run loop")
	}
}
