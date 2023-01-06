package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

var (
	testTime = time.Date(2018, time.January, 9, 14, 00, 00, 0, time.UTC)

	testLoopOutMinOnChainCltvDelta = int32(30)
	testLoopOutMaxOnChainCltvDelta = int32(40)
	testChargeOnChainCltvDelta     = int32(100)
	testSwapFee                    = btcutil.Amount(210)
	testFixedPrepayAmount          = btcutil.Amount(100)
	testMinSwapAmount              = btcutil.Amount(10000)
	testMaxSwapAmount              = btcutil.Amount(1000000)
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

	// cancelSwap is a channel that swap cancelations are sent into.
	cancelSwap chan *outCancelDetails

	lnd *test.LndMockServices
}

var _ swapServerClient = (*serverMock)(nil)

func newServerMock(lnd *test.LndMockServices) *serverMock {
	return &serverMock{
		expectedSwapAmt: 50000,

		// Total swap fee: 1000 + 0.01 * 50000 = 1050
		swapInvoiceAmt:   50950,
		prepayInvoiceAmt: 100,

		height: 600,

		preimagePush: make(chan lntypes.Preimage),
		cancelSwap:   make(chan *outCancelDetails),

		lnd: lnd,
	}
}

func (s *serverMock) NewLoopOutSwap(_ context.Context, swapHash lntypes.Hash,
	amount btcutil.Amount, _ int32, _ [33]byte, _ time.Time,
	_ string) (*newLoopOutResponse, error) {

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
	}, nil
}

func (s *serverMock) GetLoopOutTerms(ctx context.Context) (
	*LoopOutTerms, error) {

	return &LoopOutTerms{
		MinSwapAmount: testMinSwapAmount,
		MaxSwapAmount: testMaxSwapAmount,
		MinCltvDelta:  testLoopOutMinOnChainCltvDelta,
		MaxCltvDelta:  testLoopOutMaxOnChainCltvDelta,
	}, nil
}

func (s *serverMock) GetLoopOutQuote(ctx context.Context, amt btcutil.Amount,
	expiry int32, _ time.Time) (*LoopOutQuote, error) {

	dest := [33]byte{1, 2, 3}

	return &LoopOutQuote{
		SwapFee:         testSwapFee,
		SwapPaymentDest: dest,
		PrepayAmount:    testFixedPrepayAmount,
	}, nil
}

func getInvoice(hash lntypes.Hash, amt btcutil.Amount, memo string) (string, error) {
	// Set different payment addresses for swap invoices.
	payAddr := [32]byte{1, 2, 3}
	if memo == swapInvoiceDesc {
		payAddr = [32]byte{3, 2, 1}
	}

	req, err := zpay32.NewInvoice(
		&chaincfg.TestNet3Params, hash, testTime,
		zpay32.Description(memo),
		zpay32.Amount(lnwire.MilliSatoshi(1000*amt)),
		zpay32.PaymentAddr(payAddr),
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

func (s *serverMock) NewLoopInSwap(_ context.Context, swapHash lntypes.Hash,
	amount btcutil.Amount, _, _ [33]byte, swapInvoice, _ string,
	_ *route.Vertex, _ string) (*newLoopInResponse, error) {

	_, receiverKey := test.CreateKey(101)
	_, receiverInternalKey := test.CreateKey(102)

	if amount != s.expectedSwapAmt {
		return nil, errors.New("unexpected test swap amount")
	}

	var receiverKeyArray, receiverInternalKeyArray [33]byte
	copy(receiverKeyArray[:], receiverKey.SerializeCompressed())
	copy(
		receiverInternalKeyArray[:],
		receiverInternalKey.SerializeCompressed(),
	)

	s.swapInvoice = swapInvoice
	s.swapHash = swapHash

	// Simulate the server paying the probe invoice and expect the client to
	// cancel the probe payment.
	probeSub := <-s.lnd.SingleInvoiceSubcribeChannel
	probeSub.Update <- lndclient.InvoiceUpdate{
		State: invpkg.ContractAccepted,
	}
	<-s.lnd.FailInvoiceChannel

	resp := &newLoopInResponse{
		expiry:              s.height + testChargeOnChainCltvDelta,
		receiverKey:         receiverKeyArray,
		receiverInternalKey: receiverInternalKeyArray,
	}

	return resp, nil
}

func (s *serverMock) PushLoopOutPreimage(_ context.Context,
	preimage lntypes.Preimage) error {

	// Push the preimage into the mock's preimage channel.
	s.preimagePush <- preimage

	return nil
}

// CancelLoopOutSwap pushes a request to cancel a swap into our mock's channel.
func (s *serverMock) CancelLoopOutSwap(ctx context.Context,
	details *outCancelDetails) error {

	s.cancelSwap <- details
	return nil
}

func (s *serverMock) assertSwapCanceled(t *testing.T, details *outCancelDetails) {
	require.Equal(t, details, <-s.cancelSwap)
}

func (s *serverMock) GetLoopInTerms(ctx context.Context) (
	*LoopInTerms, error) {

	return &LoopInTerms{
		MinSwapAmount: testMinSwapAmount,
		MaxSwapAmount: testMaxSwapAmount,
	}, nil
}

func (s *serverMock) GetLoopInQuote(context.Context, btcutil.Amount,
	route.Vertex, *route.Vertex, [][]zpay32.HopHint) (*LoopInQuote, error) {

	return &LoopInQuote{
		SwapFee:   testSwapFee,
		CltvDelta: testChargeOnChainCltvDelta,
	}, nil
}

// SubscribeLoopOutUpdates provides a mocked implementation of state
// subscriptions.
func (s *serverMock) SubscribeLoopOutUpdates(_ context.Context,
	_ lntypes.Hash) (<-chan *ServerUpdate, <-chan error, error) {

	return nil, nil, nil
}

// SubscribeLoopInUpdates provides a mocked implementation of state subscriptions.
func (s *serverMock) SubscribeLoopInUpdates(_ context.Context,
	_ lntypes.Hash) (<-chan *ServerUpdate, <-chan error, error) {

	return nil, nil, nil
}

func (s *serverMock) Probe(ctx context.Context, amt btcutil.Amount,
	pubKey route.Vertex, lastHop *route.Vertex,
	routeHints [][]zpay32.HopHint) error {

	return nil
}

func (s *serverMock) RecommendRoutingPlugin(_ context.Context, _ lntypes.Hash,
	_ [32]byte) (RoutingPluginType, error) {

	return RoutingPluginNone, nil
}

func (s *serverMock) ReportRoutingResult(_ context.Context, _ lntypes.Hash,
	_ [32]byte, _ RoutingPluginType, _ bool, _ int32, _ int64) error {

	return nil
}

func (s *serverMock) MuSig2SignSweep(_ context.Context, _ loopdb.ProtocolVersion,
	_ lntypes.Hash, _ [32]byte, _ []byte, _ []byte) ([]byte,
	[]byte, error) {

	return nil, nil, nil
}

func (s *serverMock) PushKey(_ context.Context, _ loopdb.ProtocolVersion,
	_ lntypes.Hash, _ [32]byte) error {

	return nil
}
