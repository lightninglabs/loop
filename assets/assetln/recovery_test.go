package assetln

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestNativeRateCeiling(t *testing.T) {
	for _, test := range []struct{ debit, msat uint64 }{
		{1000, 17_180_360}, {math.MaxUint64, 1},
		{math.MaxUint64, math.MaxInt64}, {1, math.MaxInt64},
	} {
		ceiling := assetRateCeiling(test.debit, test.msat, 11)
		denominator := new(
			big.Int,
		).Exp(big.NewInt(10), big.NewInt(22), nil)
		debit := func(coefficient *big.Int) *big.Int {
			value := new(big.Int).Mul(coefficient,
				new(big.Int).SetUint64(test.msat))
			return value.Div(value, denominator)
		}
		maximum := new(big.Int).SetUint64(test.debit)
		require.LessOrEqual(t, debit(ceiling).Cmp(maximum), 0)
		next := new(big.Int).Add(ceiling, big.NewInt(1))
		require.Greater(t, debit(next).Cmp(maximum), 0)
	}
}

func TestQuoteAllowsRoundedInvoiceWithoutIncreasingNativeCap(t *testing.T) {
	terms := testTerms()
	terms.MaxDebit = terms.Amount
	terms.GroupKey[0] = 3
	decoded := testDecoded(terms)
	decoded.NumMsat = 17_180_360
	quote := &rfqrpc.PeerAcceptedSellQuote{
		Id: make([]byte, 32), AssetAmount: 1000, Expiry: 2000,
		AssetSpec: &rfqrpc.AssetSpec{GroupPubKey: terms.GroupKey[1:]},
	}
	sender := Sender{
		Now:       func() time.Time { return time.Unix(1000, 0) },
		Lightning: &mockLightning{decoded: decoded},
		RFQ: &mockRFQ{quote: func(
			req *rfqrpc.AddAssetSellOrderRequest,
		) *rfqrpc.PeerAcceptedSellQuote {

			require.Equal(t, terms.GroupKey[:],
				req.AssetSpecifier.GetGroupKey())
			cap, ok := new(big.Int).SetString(
				req.AssetRateLimit.Coefficient, 10,
			)
			require.True(t, ok)
			rate := new(big.Int).Mul(big.NewInt(5_820_600),
				big.NewInt(100_000_000_000))
			require.GreaterOrEqual(t, cap.Cmp(rate), 0)
			return quote
		}},
	}
	_, err := sender.Quote(t.Context(), terms, "invoice", nil, 0)
	require.NoError(t, err)
	quote.AssetAmount++
	_, err = sender.Quote(t.Context(), terms, "invoice", nil, 0)
	require.ErrorIs(t, err, ErrEvidence)
}

type invoiceLightning struct {
	lnrpc.LightningClient
	invoice   *lnrpc.Invoice
	lookupErr error
	decoded   *lnrpc.PayReq
}

func (m *invoiceLightning) LookupInvoice(context.Context, *lnrpc.PaymentHash,
	...grpc.CallOption) (*lnrpc.Invoice, error) {

	return m.invoice, m.lookupErr
}

func (m *invoiceLightning) DecodePayReq(context.Context, *lnrpc.PayReqString,
	...grpc.CallOption) (*lnrpc.PayReq, error) {

	return m.decoded, nil
}

func (m *invoiceLightning) ListChannels(context.Context,
	*lnrpc.ListChannelsRequest, ...grpc.CallOption) (
	*lnrpc.ListChannelsResponse, error) {

	return nil, nil
}

type settleInvoices struct {
	invoicesrpc.InvoicesClient
	err error
}

func (m *settleInvoices) SettleInvoice(context.Context,
	*invoicesrpc.SettleInvoiceMsg, ...grpc.CallOption) (
	*invoicesrpc.SettleInvoiceResp, error) {

	return nil, m.err
}

func TestSettledUsesPersistedCircuitAfterChannelClose(t *testing.T) {
	terms := testTerms()
	terms.GroupKey[0] = 3
	invoice, channels := testEvidence(t, terms)
	evidence, err := Accepted(terms, invoice, channels, 100)
	require.NoError(t, err)
	invoice.State = lnrpc.Invoice_SETTLED
	invoice.Htlcs[0].State = lnrpc.InvoiceHTLCState_SETTLED
	receiver := Receiver{Lightning: &invoiceLightning{invoice: invoice}}
	settled, err := receiver.Settled(t.Context(), terms, *evidence)
	require.NoError(t, err)
	require.True(t, settled)
	invoice.Htlcs[0].HtlcIndex++
	_, err = receiver.Settled(t.Context(), terms, *evidence)
	require.ErrorIs(t, err, ErrEvidence)
	invoice.Htlcs[0].HtlcIndex--
	evidence.GroupKey[0] = 2
	_, err = receiver.Settled(t.Context(), terms, *evidence)
	require.ErrorIs(t, err, ErrEvidence)
}

func TestReceiverIncompleteResponsesFailClosed(t *testing.T) {
	preimage := [32]byte{1}
	hash := sha256.Sum256(preimage[:])
	lost := errors.New("lost settlement response")
	ln := &invoiceLightning{}
	receiver := Receiver{
		Lightning: ln, Invoices: &settleInvoices{err: lost},
	}
	require.ErrorIs(t, receiver.Settle(t.Context(), hash, preimage), lost)
	ln.invoice = &lnrpc.Invoice{State: lnrpc.Invoice_SETTLED}
	require.ErrorIs(t, receiver.Settle(t.Context(), hash, preimage), lost)
	ln.invoice.RHash = hash[:]
	require.NoError(t, receiver.Settle(t.Context(), hash, preimage))
	_, err := receiver.Prepared(t.Context(), testTerms(), 100)
	require.ErrorIs(t, err, ErrEvidence)
}
