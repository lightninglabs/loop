package deposit

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/lndclient"
	"github.com/stretchr/testify/require"
)

// TestManagerRetriesDeferredBlockNotification checks that a block whose
// notification is deferred because lnd's tip is changing still reaches the
// deposits once the tip settles, without another block arriving.
func TestManagerRetriesDeferredBlockNotification(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const timeout = 30 * time.Second

	testContext := newManagerTestContext(t)
	manager := testContext.manager

	// While tipChanging is set, lnd reports that it isn't synced, so the
	// manager defers the block notification.
	var tipChanging atomic.Bool
	manager.cfg.LightningClient = &testLightningClient{
		getInfo: func(context.Context) (*lndclient.Info, error) {
			return &lndclient.Info{
				BlockHeight:   manager.currentHeight.Load(),
				BestBlockHash: chainhash.Hash{1},
				SyncedToChain: !tipChanging.Load(),
			}, nil
		},
	}

	initChan := make(chan struct{})
	runErrChan := make(chan error, 1)
	go func() {
		runErrChan <- manager.Run(ctx, initChan)
	}()

	testContext.blockChan <- int32(defaultDepositConfirmations)

	select {
	case <-initChan:
	case err := <-runErrChan:
		require.NoError(t, err, "manager failed to start")

	case <-time.After(timeout):
		t.Fatal("manager timed out starting")
	}

	// The expiry block arrives while lnd's tip is changing, and it is the
	// last block.
	tipChanging.Store(true)
	testContext.blockChan <- int32(
		defaultDepositConfirmations + defaultExpiry,
	)

	select {
	case <-testContext.mockLnd.SignOutputRawChannel:
		t.Fatal("deposit expired while lnd's tip was changing")

	case <-time.After(2 * minNotifyRetryInterval):
	}

	// Once the tip settles, the retried notification expires the deposit.
	tipChanging.Store(false)

	select {
	case <-testContext.mockLnd.SignOutputRawChannel:

	case <-time.After(timeout):
		t.Fatal("deferred block notification was never retried")
	}

	select {
	case <-testContext.mockLnd.TxPublishChannel:

	case <-time.After(timeout):
		t.Fatal("did not receive published expiry tx")
	}
}

// TestNextNotifyRetryDelay checks that the retry delay starts at the minimum
// and doubles up to the poll interval.
func TestNextNotifyRetryDelay(t *testing.T) {
	delay := nextNotifyRetryDelay(0)
	require.Equal(t, minNotifyRetryInterval, delay)

	delay = nextNotifyRetryDelay(delay)
	require.Equal(t, 2*minNotifyRetryInterval, delay)

	for range 10 {
		delay = nextNotifyRetryDelay(delay)
	}
	require.Equal(t, PollInterval, delay)
}
