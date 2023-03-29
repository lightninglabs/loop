package test

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

// Context contains shared test context functions.
type Context struct {
	T              *testing.T
	Lnd            *LndMockServices
	FailedInvoices map[lntypes.Hash]struct{}
	PaidInvoices   map[string]func(error)
}

// NewContext instanties a new common test context.
func NewContext(t *testing.T,
	lnd *LndMockServices) Context {

	return Context{
		T:              t,
		Lnd:            lnd,
		FailedInvoices: make(map[lntypes.Hash]struct{}),
		PaidInvoices:   make(map[string]func(error)),
	}
}

// ReceiveTx receives and decodes a published tx.
func (ctx *Context) ReceiveTx() *wire.MsgTx {
	ctx.T.Helper()

	select {
	case tx := <-ctx.Lnd.TxPublishChannel:
		return tx
	case <-time.After(Timeout):
		ctx.T.Fatalf("sweep not published")
		return nil
	}
}

// NotifySpend simulates a spend.
func (ctx *Context) NotifySpend(tx *wire.MsgTx, inputIndex uint32) {
	ctx.T.Helper()

	txHash := tx.TxHash()

	select {
	case ctx.Lnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpendingTx:        tx,
		SpenderTxHash:     &txHash,
		SpenderInputIndex: inputIndex,
	}:
	case <-time.After(Timeout):
		ctx.T.Fatalf("htlc spend not consumed")
	}
}

// NotifyConf simulates a conf.
func (ctx *Context) NotifyConf(tx *wire.MsgTx) {
	ctx.T.Helper()

	select {
	case ctx.Lnd.ConfChannel <- &chainntnfs.TxConfirmation{
		Tx: tx,
	}:
	case <-time.After(Timeout):
		ctx.T.Fatalf("htlc spend not consumed")
	}
}

// AssertRegisterSpendNtfn asserts that a register for spend has been received.
func (ctx *Context) AssertRegisterSpendNtfn(script []byte) {
	ctx.T.Helper()

	select {
	case spendIntent := <-ctx.Lnd.RegisterSpendChannel:
		require.Equal(
			ctx.T, script, spendIntent.PkScript,
			"server not listening for published htlc script",
		)

	case <-time.After(Timeout):
		DumpGoroutines()
		ctx.T.Fatalf("spend not subscribed to")
	}
}

// AssertTrackPayment asserts that a call was made to track payment, and
// returns the track payment message so that it can be used to send updates
// to the test.
func (ctx *Context) AssertTrackPayment() TrackPaymentMessage {
	ctx.T.Helper()

	var msg TrackPaymentMessage
	select {
	case msg = <-ctx.Lnd.TrackPaymentChannel:

	case <-time.After(Timeout):
		DumpGoroutines()
		ctx.T.Fatalf("payment not tracked")
	}

	return msg
}

// AssertRegisterConf asserts that a register for conf has been received.
func (ctx *Context) AssertRegisterConf(expectTxHash bool, confs int32) *ConfRegistration {
	ctx.T.Helper()

	// Expect client to register for conf
	var confIntent *ConfRegistration
	select {
	case confIntent = <-ctx.Lnd.RegisterConfChannel:
		switch {
		case expectTxHash && confIntent.TxID == nil:
			ctx.T.Fatalf("expected tx id for registration")

		case !expectTxHash && confIntent.TxID != nil:
			ctx.T.Fatalf("expected script only registration")
		}

		// Require that we registered for the number of confirmations
		// the test expects.
		require.Equal(ctx.T, confs, confIntent.NumConfs)

	case <-time.After(Timeout):
		ctx.T.Fatalf("htlc confirmed not subscribed to")
	}

	return confIntent
}

// AssertPaid asserts that the expected payment request has been paid. This
// function returns a complete function to signal the final payment result.
func (ctx *Context) AssertPaid(
	expectedMemo string) func(error) {

	ctx.T.Helper()

	if done, ok := ctx.PaidInvoices[expectedMemo]; ok {
		return done
	}

	// Assert that client pays swap invoice.
	for {
		var swapPayment RouterPaymentChannelMessage
		select {
		case swapPayment = <-ctx.Lnd.RouterSendPaymentChannel:
		case <-time.After(Timeout):
			ctx.T.Fatalf("no payment sent for invoice: %v",
				expectedMemo)
		}

		payReq := ctx.DecodeInvoice(swapPayment.SendPaymentRequest.Invoice)

		_, ok := ctx.PaidInvoices[*payReq.Description]
		require.False(
			ctx.T, ok,
			"duplicate invoice paid: %v", *payReq.Description,
		)

		done := func(result error) {
			if result != nil {
				swapPayment.Errors <- result
				return
			}
			swapPayment.Updates <- lndclient.PaymentStatus{
				State: lnrpc.Payment_SUCCEEDED,
			}
		}

		ctx.PaidInvoices[*payReq.Description] = done

		if *payReq.Description == expectedMemo {
			return done
		}
	}
}

// AssertSettled asserts that an invoice with the given hash is settled.
func (ctx *Context) AssertSettled(
	expectedHash lntypes.Hash) lntypes.Preimage {

	ctx.T.Helper()

	select {
	case preimage := <-ctx.Lnd.SettleInvoiceChannel:
		hash := sha256.Sum256(preimage[:])
		require.Equal(
			ctx.T, expectedHash, lntypes.Hash(hash),
			"server claims with wrong preimage",
		)

		return preimage
	case <-time.After(Timeout):
	}
	ctx.T.Fatalf("invoice not settled")
	return lntypes.Preimage{}
}

// AssertFailed asserts that an invoice with the given hash is failed.
func (ctx *Context) AssertFailed(expectedHash lntypes.Hash) {
	ctx.T.Helper()

	if _, ok := ctx.FailedInvoices[expectedHash]; ok {
		return
	}

	for {
		select {
		case hash := <-ctx.Lnd.FailInvoiceChannel:
			ctx.FailedInvoices[expectedHash] = struct{}{}
			if expectedHash == hash {
				return
			}
		case <-time.After(Timeout):
			ctx.T.Fatalf("invoice not failed")
		}
	}
}

// DecodeInvoice decodes a payment request string.
func (ctx *Context) DecodeInvoice(request string) *zpay32.Invoice {
	ctx.T.Helper()

	payReq, err := ctx.Lnd.DecodeInvoice(request)
	require.NoError(ctx.T, err)

	return payReq
}

// GetOutputIndex returns the index in the tx outs of the given script hash.
func (ctx *Context) GetOutputIndex(tx *wire.MsgTx,
	script []byte) int {

	for idx, out := range tx.TxOut {
		if bytes.Equal(out.PkScript, script) {
			return idx
		}
	}

	ctx.T.Fatal("htlc not present in tx")
	return 0
}

// NotifyServerHeight notifies the server of the arrival of a new block and
// waits for the notification to be processed by selecting on a
// dedicated test channel.
func (ctx *Context) NotifyServerHeight(height int32) {
	require.NoError(ctx.T, ctx.Lnd.NotifyHeight(height))
}
