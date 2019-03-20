package loop

import (
	"context"
	"errors"

	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

var (
	testTime = time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)

	testLoopOutOnChainCltvDelta = int32(30)
	testChargeOnChainCltvDelta  = int32(100)
	testCltvDelta               = 50
	testSwapFeeBase             = btcutil.Amount(21)
	testSwapFeeRate             = int64(100)
	testInvoiceExpiry           = 180 * time.Second
	testFixedPrepayAmount       = btcutil.Amount(100)
	testMinSwapAmount           = btcutil.Amount(10000)
	testMaxSwapAmount           = btcutil.Amount(1000000)
	testTxConfTarget            = 2
	testRepublishDelay          = 10 * time.Second
)

// serverMock is used in client unit tests to simulate swap server behaviour.
type serverMock struct {
	t *testing.T

	expectedSwapAmt  btcutil.Amount
	swapInvoiceAmt   btcutil.Amount
	prepayInvoiceAmt btcutil.Amount

	height int32

	swapInvoice string
	swapHash    lntypes.Hash
}

func newServerMock() *serverMock {
	return &serverMock{
		expectedSwapAmt: 50000,

		// Total swap fee: 1000 + 0.01 * 50000 = 1050
		swapInvoiceAmt:   50950,
		prepayInvoiceAmt: 100,

		height: 600,
	}
}

func (s *serverMock) NewLoopOutSwap(ctx context.Context,
	swapHash lntypes.Hash, amount btcutil.Amount,
	receiverKey [33]byte) (
	*newLoopOutResponse, error) {

	_, senderKey := test.CreateKey(100)

	if amount != s.expectedSwapAmt {
		return nil, errors.New("unexpected test swap amount")
	}

	swapPayReqString, err := getInvoice(swapHash, s.swapInvoiceAmt,
		swapInvoiceDesc)
	if err != nil {
		return nil, err
	}

	prePayReqString, err := getInvoice(swapHash, s.prepayInvoiceAmt,
		prepayInvoiceDesc)
	if err != nil {
		return nil, err
	}

	var senderKeyArray [33]byte
	copy(senderKeyArray[:], senderKey.SerializeCompressed())

	return &newLoopOutResponse{
		senderKey:     senderKeyArray,
		swapInvoice:   swapPayReqString,
		prepayInvoice: prePayReqString,
		expiry:        s.height + testLoopOutOnChainCltvDelta,
	}, nil
}

func (s *serverMock) GetLoopOutTerms(ctx context.Context) (
	*LoopOutTerms, error) {

	dest := [33]byte{1, 2, 3}

	return &LoopOutTerms{
		SwapFeeBase:     testSwapFeeBase,
		SwapFeeRate:     testSwapFeeRate,
		SwapPaymentDest: dest,
		CltvDelta:       testLoopOutOnChainCltvDelta,
		MinSwapAmount:   testMinSwapAmount,
		MaxSwapAmount:   testMaxSwapAmount,
		PrepayAmt:       testFixedPrepayAmount,
	}, nil
}

func getInvoice(hash lntypes.Hash, amt btcutil.Amount, memo string) (string, error) {
	req, err := zpay32.NewInvoice(
		&chaincfg.TestNet3Params, hash, testTime,
		zpay32.Description(memo),
		zpay32.Amount(lnwire.MilliSatoshi(1000*amt)),
	)
	if err != nil {
		return "", err
	}

	reqString, err := test.EncodePayReq(req)
	if err != nil {
		return "", err
	}

	return reqString, nil
}

func (s *serverMock) NewLoopInSwap(ctx context.Context,
	swapHash lntypes.Hash, amount btcutil.Amount,
	senderKey [33]byte, swapInvoice string) (
	*newLoopInResponse, error) {

	_, receiverKey := test.CreateKey(101)

	if amount != s.expectedSwapAmt {
		return nil, errors.New("unexpected test swap amount")
	}

	prepayHash := lntypes.Hash{1, 2, 3}

	prePayReqString, err := getInvoice(prepayHash, s.prepayInvoiceAmt,
		prepayInvoiceDesc)
	if err != nil {
		return nil, err
	}

	var receiverKeyArray [33]byte
	copy(receiverKeyArray[:], receiverKey.SerializeCompressed())

	s.swapInvoice = swapInvoice
	s.swapHash = swapHash

	resp := &newLoopInResponse{
		expiry:        s.height + testChargeOnChainCltvDelta,
		receiverKey:   receiverKeyArray,
		prepayInvoice: prePayReqString,
	}

	return resp, nil
}

func (s *serverMock) GetLoopInTerms(ctx context.Context) (
	*LoopInTerms, error) {

	return &LoopInTerms{
		SwapFeeBase:   testSwapFeeBase,
		SwapFeeRate:   testSwapFeeRate,
		CltvDelta:     testChargeOnChainCltvDelta,
		MinSwapAmount: testMinSwapAmount,
		MaxSwapAmount: testMaxSwapAmount,
		PrepayAmt:     testFixedPrepayAmount,
	}, nil
}
