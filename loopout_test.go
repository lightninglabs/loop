package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/test"
)

// TestLateHtlcPublish tests that the client is not revealing the preimage if
// there are not enough blocks left.
func TestLateHtlcPublish(t *testing.T) {
	defer test.Guard(t)()

	lnd := test.NewMockLnd()

	ctx := test.NewContext(t, lnd)

	server := newServerMock()

	store := newStoreMock(t)

	expiryChan := make(chan time.Time)
	timerFactory := func(expiry time.Duration) <-chan time.Time {
		return expiryChan
	}

	height := int32(600)

	cfg := &swapConfig{
		lnd:    &lnd.LndServices,
		store:  store,
		server: server,
	}

	swap, err := newLoopOutSwap(
		context.Background(), cfg, height, testRequest,
	)
	if err != nil {
		t.Fatal(err)
	}

	sweeper := &sweep.Sweeper{Lnd: &lnd.LndServices}

	blockEpochChan := make(chan interface{})
	statusChan := make(chan SwapInfo)

	errChan := make(chan error)
	go func() {
		err := swap.execute(context.Background(), &executeConfig{
			statusChan:     statusChan,
			sweeper:        sweeper,
			blockEpochChan: blockEpochChan,
			timerFactory:   timerFactory,
		}, height)
		if err != nil {
			logger.Error(err)
		}
		errChan <- err
	}()

	store.assertLoopOutStored()

	state := <-statusChan
	if state.State != loopdb.StateInitiated {
		t.Fatal("unexpected state")
	}

	signalSwapPaymentResult := ctx.AssertPaid(swapInvoiceDesc)
	signalPrepaymentResult := ctx.AssertPaid(prepayInvoiceDesc)

	// Expect client to register for conf
	ctx.AssertRegisterConf()

	// // Wait too long before publishing htlc.
	blockEpochChan <- int32(swap.CltvExpiry - 10)

	signalSwapPaymentResult(
		errors.New(lndclient.PaymentResultUnknownPaymentHash),
	)
	signalPrepaymentResult(
		errors.New(lndclient.PaymentResultUnknownPaymentHash),
	)

	store.assertStoreFinished(loopdb.StateFailTimeout)

	status := <-statusChan
	if status.State != loopdb.StateFailTimeout {
		t.Fatal("unexpected state")
	}

	err = <-errChan
	if err != nil {
		t.Fatal(err)
	}
}
