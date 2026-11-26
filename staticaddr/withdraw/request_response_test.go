package withdraw

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDeliverWithdrawalRequestAbandonedCaller checks that the manager's run
// loop can still hand over its response after the caller of
// DeliverWithdrawalRequest stopped waiting. A response that cannot be
// delivered would block every later withdrawal request.
func TestDeliverWithdrawalRequestAbandonedCaller(t *testing.T) {
	m := &Manager{
		newWithdrawalRequestChan: make(chan newWithdrawalRequest),
		exitChan:                 make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := m.DeliverWithdrawalRequest(ctx, nil, "", 0, 0)
		done <- err
	}()

	// Take the request like the run loop does.
	var request newWithdrawalRequest
	select {
	case request = <-m.newWithdrawalRequestChan:
	case <-time.After(time.Second):
		require.FailNow(t, "request was not delivered")
	}

	// The caller goes away while the withdrawal is still being created.
	cancel()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "context canceled")
	case <-time.After(time.Second):
		require.FailNow(t, "caller did not return")
	}

	// The run loop must be able to deliver its response regardless.
	select {
	case request.respChan <- &newWithdrawalResponse{}:
	case <-time.After(time.Second):
		require.FailNow(t, "response to an abandoned request blocks "+
			"the manager run loop")
	}
}
