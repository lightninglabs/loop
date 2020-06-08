package loop

import (
	"context"
	"errors"

	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

var (
	testTime = time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)

	testLoopOutOnChainCltvDelta = int32(30)
	testChargeOnChainCltvDelta  = int32(100)
	testSwapFee                 = btcutil.Amount(210)
	testFixedPrepayAmount       = btcutil.Amount(100)
	testMinSwapAmount           = btcutil.Amount(10000)
	testMaxSwapAmount           = btcutil.Amount(1000000)
)

// serverMock is used in client unit tests to simulate swap server behaviour.
type serverMock struct {
	expectedSwapAmt  btcutil.Amount
	swapInvoiceAmt   btcutil.Amount
	prepayInvoiceAmt btcutil.Amount

	height int32

	swapInvoice string
	swapHash    lntypes.Hash

	// preimagePush is a channel that preimage pushes are sent into.
	preimagePush chan lntypes.Preimage
}

var _ swapServerClient = (*serverMock)(nil)

func newServerMock() *serverMock {
	return &serverMock{
		expectedSwapAmt: 50000,

		// Total swap fee: 1000 + 0.01 * 50000 = 1050
		swapInvoiceAmt:   50950,
		prepayInvoiceAmt: 100,

		height: 600,

		preimagePush: make(chan lntypes.Preimage),
	}
}

func (s *serverMock) NewLoopOutSwap(ctx context.Context,
	swapHash lntypes.Hash, amount btcutil.Amount,
	receiverKey [33]byte, _ time.Time) (
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

	return &LoopOutTerms{
		MinSwapAmount: testMinSwapAmount,
		MaxSwapAmount: testMaxSwapAmount,
	}, nil
}

func (s *serverMock) GetLoopOutQuote(ctx context.Context, amt btcutil.Amount,
	_ time.Time) (*LoopOutQuote, error) {

	dest := [33]byte{1, 2, 3}

	return &LoopOutQuote{
		SwapFee:         testSwapFee,
		SwapPaymentDest: dest,
		CltvDelta:       testLoopOutOnChainCltvDelta,
		PrepayAmount:    testFixedPrepayAmount,
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
	senderKey [33]byte, swapInvoice string, lastHop *route.Vertex) (
	*newLoopInResponse, error) {

	_, receiverKey := test.CreateKey(101)

	if amount != s.expectedSwapAmt {
		return nil, errors.New("unexpected test swap amount")
	}

	var receiverKeyArray [33]byte
	copy(receiverKeyArray[:], receiverKey.SerializeCompressed())

	s.swapInvoice = swapInvoice
	s.swapHash = swapHash

	resp := &newLoopInResponse{
		expiry:      s.height + testChargeOnChainCltvDelta,
		receiverKey: receiverKeyArray,
	}

	return resp, nil
}

func (s *serverMock) PushLoopOutPreimage(_ context.Context,
	preimage lntypes.Preimage) error {

	// Push the preimage into the mock's preimage channel.
	s.preimagePush <- preimage

	return nil
}

func (s *serverMock) GetLoopInTerms(ctx context.Context) (
	*LoopInTerms, error) {

	return &LoopInTerms{
		MinSwapAmount: testMinSwapAmount,
		MaxSwapAmount: testMaxSwapAmount,
	}, nil
}

func (s *serverMock) GetLoopInQuote(ctx context.Context, amt btcutil.Amount) (
	*LoopInQuote, error) {

	return &LoopInQuote{
		SwapFee:   testSwapFee,
		CltvDelta: testChargeOnChainCltvDelta,
	}, nil
}
