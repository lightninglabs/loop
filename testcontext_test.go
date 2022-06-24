package loop

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

var (
	testPreimage = lntypes.Preimage([32]byte{
		1, 1, 1, 1, 2, 2, 2, 2,
		3, 3, 3, 3, 4, 4, 4, 4,
		1, 1, 1, 1, 2, 2, 2, 2,
		3, 3, 3, 3, 4, 4, 4, 4,
	})
)

// testContext contains functionality to support client unit tests.
type testContext struct {
	test.Context

	serverMock *serverMock
	swapClient *Client
	statusChan chan SwapInfo
	store      *storeMock
	expiryChan chan time.Time
	runErr     chan error
	stop       func()
}

// mockVerifySchnorrSigFail is used to simulate failed taproot keyspend
// signature verification. If passed to the executeConfig we'll test an
// uncooperative server and will fall back to scriptspend sweep.
func mockVerifySchnorrSigFail(pubKey *btcec.PublicKey, hash,
	sig []byte) error {

	return fmt.Errorf("invalid sig")
}

func newSwapClient(config *clientConfig) *Client {
	sweeper := &sweep.Sweeper{
		Lnd: config.LndServices,
	}

	lndServices := config.LndServices

	executor := newExecutor(&executorConfig{
		lnd:               lndServices,
		store:             config.Store,
		sweeper:           sweeper,
		createExpiryTimer: config.CreateExpiryTimer,
		cancelSwap:        config.Server.CancelLoopOutSwap,
		verifySchnorrSig:  mockVerifySchnorrSigFail,
	})

	return &Client{
		errChan:      make(chan error),
		clientConfig: *config,
		lndServices:  lndServices,
		sweeper:      sweeper,
		executor:     executor,
		resumeReady:  make(chan struct{}),
	}
}

func createClientTestContext(t *testing.T,
	pendingSwaps []*loopdb.LoopOut) *testContext {

	clientLnd := test.NewMockLnd()
	serverMock := newServerMock(clientLnd)

	store := newStoreMock(t)
	for _, s := range pendingSwaps {
		store.loopOutSwaps[s.Hash] = s.Contract

		updates := []loopdb.SwapStateData{}
		for _, e := range s.Events {
			updates = append(updates, e.SwapStateData)
		}
		store.loopOutUpdates[s.Hash] = updates
	}

	expiryChan := make(chan time.Time)
	timerFactory := func(expiry time.Duration) <-chan time.Time {
		return expiryChan
	}

	swapClient := newSwapClient(&clientConfig{
		LndServices:       &clientLnd.LndServices,
		Server:            serverMock,
		Store:             store,
		CreateExpiryTimer: timerFactory,
	})

	statusChan := make(chan SwapInfo)

	ctx := &testContext{
		Context: test.NewContext(
			t,
			clientLnd,
		),
		swapClient: swapClient,
		statusChan: statusChan,
		expiryChan: expiryChan,
		store:      store,
		serverMock: serverMock,
	}

	ctx.runErr = make(chan error)
	runCtx, stop := context.WithCancel(context.Background())
	ctx.stop = stop

	go func() {
		err := swapClient.Run(
			runCtx,
			statusChan,
		)
		log.Errorf("client run: %v", err)
		ctx.runErr <- err
	}()

	return ctx
}

func (ctx *testContext) finish() {
	ctx.stop()
	select {
	case err := <-ctx.runErr:
		if err != nil {
			ctx.T.Fatal(err)
		}
	case <-time.After(test.Timeout):
		ctx.T.Fatal("client not stopping")
	}

	ctx.assertIsDone()
}

// notifyHeight notifies swap client of the arrival of a new block and
// waits for the notification to be processed by selecting on a dedicated
// test channel.
func (ctx *testContext) notifyHeight(height int32) {
	ctx.T.Helper()

	if err := ctx.Lnd.NotifyHeight(height); err != nil {
		ctx.T.Fatal(err)
	}
}

func (ctx *testContext) assertIsDone() {
	if err := ctx.Lnd.IsDone(); err != nil {
		ctx.T.Fatal(err)
	}

	if err := ctx.store.isDone(); err != nil {
		ctx.T.Fatal(err)
	}

	select {
	case <-ctx.statusChan:
		ctx.T.Fatalf("not all status updates read")
	default:
	}
}

func (ctx *testContext) assertStored() {
	ctx.T.Helper()

	ctx.store.assertLoopOutStored()
}

func (ctx *testContext) assertStorePreimageReveal() {
	ctx.T.Helper()

	ctx.store.assertStorePreimageReveal()
}

func (ctx *testContext) assertStoreFinished(expectedResult loopdb.SwapState) {
	ctx.T.Helper()

	ctx.store.assertStoreFinished(expectedResult)

}

func (ctx *testContext) assertStatus(expectedState loopdb.SwapState) {

	ctx.T.Helper()

	for {
		select {
		case update := <-ctx.statusChan:
			if update.SwapType != swap.TypeOut {
				continue
			}

			if update.State == expectedState {
				return
			}
		case <-time.After(test.Timeout):
			ctx.T.Fatalf("expected status %v not "+
				"received in time", expectedState)
		}
	}
}

func (ctx *testContext) publishHtlc(script []byte,
	amt btcutil.Amount) wire.OutPoint {

	// Create the htlc tx.
	htlcTx := wire.MsgTx{}
	htlcTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{},
	})
	htlcTx.AddTxOut(&wire.TxOut{
		PkScript: script,
		Value:    int64(amt),
	})

	htlcTxHash := htlcTx.TxHash()

	// Signal client that script has been published.
	select {
	case ctx.Lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: &htlcTx,
	}:
	case <-time.After(test.Timeout):
		ctx.T.Fatalf("htlc confirmed not consumed")
	}

	return wire.OutPoint{
		Hash:  htlcTxHash,
		Index: 0,
	}
}

// trackPayment asserts that a call to track payment was sent and sends the
// status provided into the updates channel.
func (ctx *testContext) trackPayment(status lnrpc.Payment_PaymentStatus) {
	trackPayment := ctx.Context.AssertTrackPayment()

	select {
	case trackPayment.Updates <- lndclient.PaymentStatus{
		State: status,
	}:

	case <-time.After(test.Timeout):
		ctx.T.Fatalf("could not send payment update")
	}
}

// assertPreimagePush asserts that we made an attempt to push our preimage to
// the server.
func (ctx *testContext) assertPreimagePush(preimage lntypes.Preimage) {
	select {
	case pushedPreimage := <-ctx.serverMock.preimagePush:
		require.Equal(ctx.T, preimage, pushedPreimage)

	case <-time.After(test.Timeout):
		ctx.T.Fatalf("preimage not pushed")
	}
}
