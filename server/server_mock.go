package server

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
	TestTime = time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)

	TestLoopOutMinOnChainCltvDelta = int32(30)
	testLoopOutMaxOnChainCltvDelta = int32(40)
	testChargeOnChainCltvDelta     = int32(100)
	testSwapFee                    = btcutil.Amount(210)
	testFixedPrepayAmount          = btcutil.Amount(100)
	testMinSwapAmount              = btcutil.Amount(10000)
	testMaxSwapAmount              = btcutil.Amount(1000000)

	// SwapInvoiceDesc is the description we give swap invoices.
	SwapInvoiceDesc = "swap"

	// PrepayInvoiceDesc is the description we give prepays.
	PrepayInvoiceDesc = "prepay"
)

// Mock is used in client unit tests to simulate swap server behaviour.
type Mock struct {
	expectedSwapAmt  btcutil.Amount
	SwapInvoiceAmt   btcutil.Amount
	PrepayInvoiceAmt btcutil.Amount

	height int32

	swapInvoice string
	SwapHash    lntypes.Hash

	// PreimagePush is a channel that preimage pushes are sent into.
	PreimagePush chan lntypes.Preimage
}

var _ SwapServerClient = (*Mock)(nil)

// NewServerMock returns a mocked server.
func NewServerMock() *Mock {
	return &Mock{
		expectedSwapAmt: 50000,

		// Total swap fee: 1000 + 0.01 * 50000 = 1050
		SwapInvoiceAmt:   50950,
		PrepayInvoiceAmt: 100,

		height: 600,

		PreimagePush: make(chan lntypes.Preimage),
	}
}

func (s *Mock) NewLoopOutSwap(ctx context.Context,
	swapHash lntypes.Hash, amount btcutil.Amount, _ int32, _ [33]byte,
	_ time.Time) (*NewLoopOutResponse, error) {

	_, senderKey := test.CreateKey(100)

	if amount != s.expectedSwapAmt {
		return nil, errors.New("unexpected test swap amount")
	}

	swapPayReqString, err := GetInvoice(swapHash, s.SwapInvoiceAmt,
		SwapInvoiceDesc)
	if err != nil {
		return nil, err
	}

	prePayReqString, err := GetInvoice(swapHash, s.PrepayInvoiceAmt,
		PrepayInvoiceDesc)
	if err != nil {
		return nil, err
	}

	var senderKeyArray [33]byte
	copy(senderKeyArray[:], senderKey.SerializeCompressed())

	return &NewLoopOutResponse{
		SenderKey:     senderKeyArray,
		SwapInvoice:   swapPayReqString,
		PrepayInvoice: prePayReqString,
	}, nil
}

func (s *Mock) GetLoopOutTerms(_ context.Context) (
	*LoopOutTerms, error) {

	return &LoopOutTerms{
		MinSwapAmount: testMinSwapAmount,
		MaxSwapAmount: testMaxSwapAmount,
		MinCltvDelta:  TestLoopOutMinOnChainCltvDelta,
		MaxCltvDelta:  testLoopOutMaxOnChainCltvDelta,
	}, nil
}

func (s *Mock) GetLoopOutQuote(_ context.Context, _ btcutil.Amount,
	_ int32, _ time.Time) (*LoopOutQuote, error) {

	dest := [33]byte{1, 2, 3}

	return &LoopOutQuote{
		SwapFee:         testSwapFee,
		SwapPaymentDest: dest,
		PrepayAmount:    testFixedPrepayAmount,
	}, nil
}

func GetInvoice(hash lntypes.Hash, amt btcutil.Amount,
	memo string) (string, error) {

	req, err := zpay32.NewInvoice(
		&chaincfg.TestNet3Params, hash, TestTime,
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

func (s *Mock) NewLoopInSwap(_ context.Context,
	swapHash lntypes.Hash, amount btcutil.Amount,
	_ [33]byte, swapInvoice string, _ *route.Vertex) (
	*NewLoopInResponse, error) {

	_, receiverKey := test.CreateKey(101)

	if amount != s.expectedSwapAmt {
		return nil, errors.New("unexpected test swap amount")
	}

	var receiverKeyArray [33]byte
	copy(receiverKeyArray[:], receiverKey.SerializeCompressed())

	s.swapInvoice = swapInvoice
	s.SwapHash = swapHash

	resp := &NewLoopInResponse{
		Expiry:      s.height + testChargeOnChainCltvDelta,
		ReceiverKey: receiverKeyArray,
	}

	return resp, nil
}

func (s *Mock) PushLoopOutPreimage(_ context.Context,
	preimage lntypes.Preimage) error {

	// Push the preimage into the mock's preimage channel.
	s.PreimagePush <- preimage

	return nil
}

func (s *Mock) GetLoopInTerms(_ context.Context) (
	*LoopInTerms, error) {

	return &LoopInTerms{
		MinSwapAmount: testMinSwapAmount,
		MaxSwapAmount: testMaxSwapAmount,
	}, nil
}

func (s *Mock) GetLoopInQuote(_ context.Context, _ btcutil.Amount) (
	*LoopInQuote, error) {

	return &LoopInQuote{
		SwapFee:   testSwapFee,
		CltvDelta: testChargeOnChainCltvDelta,
	}, nil
}

// SubscribeLoopOutUpdates provides a mocked implementation of state
// subscriptions.
func (s *Mock) SubscribeLoopOutUpdates(_ context.Context,
	_ lntypes.Hash) (<-chan *Update, <-chan error, error) {

	return nil, nil, nil
}

// SubscribeLoopInUpdates provides a mocked implementation of state subscriptions.
func (s *Mock) SubscribeLoopInUpdates(_ context.Context,
	_ lntypes.Hash) (<-chan *Update, <-chan error, error) {

	return nil, nil, nil
}
