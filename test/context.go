package test

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
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
		if !bytes.Equal(spendIntent.PkScript, script) {
			ctx.T.Fatalf("server not listening for published htlc script")
		}
	case <-time.After(Timeout):
		DumpGoroutines()
		ctx.T.Fatalf("spend not subscribed to")
	}
}

// AssertRegisterConf asserts that a register for conf has been received.
func (ctx *Context) AssertRegisterConf() *ConfRegistration {
	ctx.T.Helper()

	// Expect client to register for conf
	var confIntent *ConfRegistration
	select {
	case confIntent = <-ctx.Lnd.RegisterConfChannel:
		if confIntent.TxID != nil {
			ctx.T.Fatalf("expected script only registration")
		}
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
		var swapPayment PaymentChannelMessage
		select {
		case swapPayment = <-ctx.Lnd.SendPaymentChannel:
		case <-time.After(Timeout):
			ctx.T.Fatalf("no payment sent for invoice: %v",
				expectedMemo)
		}

		payReq := ctx.DecodeInvoice(swapPayment.PaymentRequest)

		if _, ok := ctx.PaidInvoices[*payReq.Description]; ok {
			ctx.T.Fatalf("duplicate invoice paid: %v",
				*payReq.Description)
		}

		done := func(result error) {
			select {
			case swapPayment.Done <- result:
			case <-time.After(Timeout):
				ctx.T.Fatalf("payment result not consumed")
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
		if expectedHash != hash {
			ctx.T.Fatalf("server claims with wrong preimage")
		}

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
	if err != nil {
		ctx.T.Fatal(err)
	}
	return payReq
}

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
	if err := ctx.Lnd.NotifyHeight(height); err != nil {
		ctx.T.Fatal(err)
	}
}
