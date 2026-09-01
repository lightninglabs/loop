package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const loopOutTestHeight = int32(600)

type loopOutTestInvoiceSubscription struct {
	updates chan lndclient.InvoiceUpdate
	errors  chan error
}

type loopOutTestInvoices struct {
	lndclient.InvoicesClient

	mu            sync.Mutex
	invoiceKey    *btcec.PrivateKey
	addRequests   []*invoicesrpc.AddInvoiceData
	subscriptions map[lntypes.Hash]*loopOutTestInvoiceSubscription
	settled       map[lntypes.Hash]struct{}
	cancelErrors  map[lntypes.Hash][]error
	cancelCalls   map[lntypes.Hash]int

	subscribed chan lntypes.Hash
	settles    chan lntypes.Preimage
	cancels    chan lntypes.Hash
}

func newLoopOutTestInvoices(t *testing.T) *loopOutTestInvoices {
	t.Helper()

	privateKey, _ := btcec.PrivKeyFromBytes([]byte{1})
	return &loopOutTestInvoices{
		invoiceKey: privateKey,
		subscriptions: make(
			map[lntypes.Hash]*loopOutTestInvoiceSubscription,
		),
		settled:      make(map[lntypes.Hash]struct{}),
		cancelErrors: make(map[lntypes.Hash][]error),
		cancelCalls:  make(map[lntypes.Hash]int),
		subscribed:   make(chan lntypes.Hash, 4),
		settles:      make(chan lntypes.Preimage, 4),
		cancels:      make(chan lntypes.Hash, 8),
	}
}

func (i *loopOutTestInvoices) AddHoldInvoice(_ context.Context,
	request *invoicesrpc.AddInvoiceData) (string, error) {

	i.mu.Lock()
	requestCopy := *request
	hashCopy := *request.Hash
	requestCopy.Hash = &hashCopy
	i.addRequests = append(i.addRequests, &requestCopy)
	i.mu.Unlock()

	paymentAddr := sha256.Sum256(append(
		[]byte("payment-address"), request.Hash[:]...,
	))
	invoice, err := zpay32.NewInvoice(
		&chaincfg.RegressionNetParams, *request.Hash, time.Now(),
		zpay32.Description(request.Memo),
		zpay32.Amount(request.Value),
		zpay32.CLTVExpiry(request.CltvExpiry),
		zpay32.PaymentAddr(paymentAddr),
	)
	if err != nil {
		return "", err
	}

	return invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(digest []byte) ([]byte, error) {
			return ecdsa.SignCompact(i.invoiceKey, digest, true), nil
		},
	})
}

func (i *loopOutTestInvoices) SubscribeSingleInvoice(_ context.Context,
	hash lntypes.Hash) (<-chan lndclient.InvoiceUpdate, <-chan error, error) {

	subscription := &loopOutTestInvoiceSubscription{
		updates: make(chan lndclient.InvoiceUpdate, 4),
		errors:  make(chan error, 1),
	}
	i.mu.Lock()
	i.subscriptions[hash] = subscription
	i.mu.Unlock()
	i.subscribed <- hash

	return subscription.updates, subscription.errors, nil
}

func (i *loopOutTestInvoices) SettleInvoice(_ context.Context,
	preimage lntypes.Preimage) error {

	i.mu.Lock()
	if _, ok := i.settled[preimage.Hash()]; ok {
		i.mu.Unlock()
		return status.Error(codes.AlreadyExists, "invoice already settled")
	}
	i.settled[preimage.Hash()] = struct{}{}
	i.mu.Unlock()
	i.settles <- preimage

	return nil
}

func (i *loopOutTestInvoices) CancelInvoice(_ context.Context,
	hash lntypes.Hash) error {

	i.mu.Lock()
	i.cancelCalls[hash]++
	var cancelErr error
	if queued := i.cancelErrors[hash]; len(queued) != 0 {
		cancelErr = queued[0]
		i.cancelErrors[hash] = queued[1:]
	}
	i.mu.Unlock()
	i.cancels <- hash

	return cancelErr
}

func (i *loopOutTestInvoices) setCancelErrors(hash lntypes.Hash,
	errors ...error) {

	i.mu.Lock()
	i.cancelErrors[hash] = append([]error(nil), errors...)
	i.mu.Unlock()
}

func (i *loopOutTestInvoices) cancelCount(hash lntypes.Hash) int {
	i.mu.Lock()
	defer i.mu.Unlock()

	return i.cancelCalls[hash]
}

func (i *loopOutTestInvoices) sendState(t *testing.T, hash lntypes.Hash,
	state invpkg.ContractState) {

	t.Helper()
	i.mu.Lock()
	subscription := i.subscriptions[hash]
	i.mu.Unlock()
	require.NotNil(t, subscription)
	subscription.updates <- lndclient.InvoiceUpdate{
		Invoice: lndclient.Invoice{
			Hash:  hash,
			State: state,
		},
	}
}

func (i *loopOutTestInvoices) addCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()

	return len(i.addRequests)
}

type loopOutTestLightning struct {
	lndclient.LightningClient

	height int32
	pubKey [33]byte
}

func (l *loopOutTestLightning) GetInfo(context.Context) (*lndclient.Info,
	error) {

	return &lndclient.Info{
		BlockHeight:    uint32(l.height),
		IdentityPubkey: l.pubKey,
	}, nil
}

type loopOutTestWallet struct {
	lndclient.WalletKitClient

	mu                       sync.Mutex
	keyIndex                 uint32
	sendErr                  error
	estimateUntilContextDone bool
	attempts                 chan *wire.MsgTx
	sent                     chan *wire.MsgTx
}

func (w *loopOutTestWallet) DeriveNextKey(_ context.Context,
	family int32) (*keychain.KeyDescriptor, error) {

	w.mu.Lock()
	w.keyIndex++
	index := w.keyIndex
	w.mu.Unlock()

	keyMaterial := make([]byte, 32)
	keyMaterial[28] = byte(index >> 24)
	keyMaterial[29] = byte(index >> 16)
	keyMaterial[30] = byte(index >> 8)
	keyMaterial[31] = byte(index)
	privateKey, publicKey := btcec.PrivKeyFromBytes(keyMaterial)
	_ = privateKey

	return &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(family),
			Index:  index,
		},
		PubKey: publicKey,
	}, nil
}

func (w *loopOutTestWallet) EstimateFeeRate(ctx context.Context,
	_ int32) (chainfee.SatPerKWeight, error) {

	w.mu.Lock()
	waitForContext := w.estimateUntilContextDone
	w.mu.Unlock()
	if waitForContext {
		<-ctx.Done()
		return 0, ctx.Err()
	}

	return chainfee.SatPerKWeight(1_000), nil
}

func (w *loopOutTestWallet) SendOutputs(_ context.Context,
	outputs []*wire.TxOut, _ chainfee.SatPerKWeight,
	_ string) (*wire.MsgTx, error) {

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 1,
		},
	})
	for _, output := range outputs {
		tx.AddTxOut(&wire.TxOut{
			Value:    output.Value,
			PkScript: bytes.Clone(output.PkScript),
		})
	}
	w.attempts <- tx.Copy()
	w.mu.Lock()
	sendErr := w.sendErr
	w.mu.Unlock()
	if sendErr != nil {
		return nil, sendErr
	}
	w.sent <- tx.Copy()

	return tx, nil
}

func (w *loopOutTestWallet) failSend(err error) {
	w.mu.Lock()
	w.sendErr = err
	w.mu.Unlock()
}

func (w *loopOutTestWallet) waitForEstimateDeadline() {
	w.mu.Lock()
	w.estimateUntilContextDone = true
	w.mu.Unlock()
}

type loopOutTestConfRegistration struct {
	txid          *chainhash.Hash
	pkScript      []byte
	confirmations int32
	heightHint    int32
	confirmed     chan *chainntnfs.TxConfirmation
	errors        chan error
}

type loopOutTestSpendRegistration struct {
	outpoint   *wire.OutPoint
	pkScript   []byte
	heightHint int32
	spends     chan *chainntnfs.SpendDetail
	errors     chan error
}

type loopOutTestNotifier struct {
	lndclient.ChainNotifierClient

	registrations      chan *loopOutTestConfRegistration
	spendRegistrations chan *loopOutTestSpendRegistration
}

func (n *loopOutTestNotifier) RegisterConfirmationsNtfn(_ context.Context,
	txid *chainhash.Hash, pkScript []byte, confirmations,
	heightHint int32, _ ...lndclient.NotifierOption) (
	chan *chainntnfs.TxConfirmation, chan error, error) {

	registration := &loopOutTestConfRegistration{
		pkScript:      bytes.Clone(pkScript),
		confirmations: confirmations,
		heightHint:    heightHint,
		confirmed:     make(chan *chainntnfs.TxConfirmation, 1),
		errors:        make(chan error, 1),
	}
	if txid != nil {
		registration.txid = cloneHash(*txid)
	}
	n.registrations <- registration

	return registration.confirmed, registration.errors, nil
}

func (n *loopOutTestNotifier) RegisterSpendNtfn(_ context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint int32,
	_ ...lndclient.NotifierOption) (chan *chainntnfs.SpendDetail, chan error,
	error) {

	registration := &loopOutTestSpendRegistration{
		pkScript:   bytes.Clone(pkScript),
		heightHint: heightHint,
		spends:     make(chan *chainntnfs.SpendDetail, 1),
		errors:     make(chan error, 1),
	}
	if outpoint != nil {
		copyOutpoint := *outpoint
		registration.outpoint = &copyOutpoint
	}
	n.spendRegistrations <- registration

	return registration.spends, registration.errors, nil
}

type loopOutTestMuSigCall struct {
	version input.MuSig2Version
	locator keychain.KeyLocator
	signers [][]byte
}

type loopOutTestSigner struct {
	lndclient.SignerClient

	mu          sync.Mutex
	createCalls []loopOutTestMuSigCall
	signed      chan [32]byte
}

func (s *loopOutTestSigner) MuSig2CreateSession(_ context.Context,
	version input.MuSig2Version, locator *keychain.KeyLocator,
	signers [][]byte, _ ...lndclient.MuSig2SessionOpts) (
	*input.MuSig2SessionInfo, error) {

	signerCopies := make([][]byte, len(signers))
	for index := range signers {
		signerCopies[index] = bytes.Clone(signers[index])
	}
	s.mu.Lock()
	s.createCalls = append(s.createCalls, loopOutTestMuSigCall{
		version: version,
		locator: *locator,
		signers: signerCopies,
	})
	s.mu.Unlock()

	var publicNonce [musig2.PubNonceSize]byte
	publicNonce[0] = 2
	return &input.MuSig2SessionInfo{
		SessionID:     [32]byte{1},
		Version:       version,
		PublicNonce:   publicNonce,
		HaveAllNonces: true,
	}, nil
}

func (s *loopOutTestSigner) MuSig2Sign(_ context.Context, _ [32]byte,
	digest [32]byte, _ bool) ([]byte, error) {

	s.signed <- digest
	return make([]byte, input.MuSig2PartialSigSize), nil
}

func (s *loopOutTestSigner) MuSig2Cleanup(context.Context, [32]byte) error {
	return nil
}

func (s *loopOutTestSigner) createCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.createCalls)
}

type loopOutTestBitcoin struct{}

func (loopOutTestBitcoin) GetTxOut(*chainhash.Hash, uint32,
	bool) (*btcjson.GetTxOutResult, error) {

	return nil, nil
}

func (loopOutTestBitcoin) GetRawTransaction(
	*chainhash.Hash) (*btcutil.Tx, error) {

	return nil, nil
}

type loopOutTestHarness struct {
	server   *Server
	invoices *loopOutTestInvoices
	wallet   *loopOutTestWallet
	notifier *loopOutTestNotifier
	signer   *loopOutTestSigner
}

func newLoopOutTestHarness(t *testing.T) *loopOutTestHarness {
	t.Helper()

	_, identityKey := btcec.PrivKeyFromBytes([]byte{9})
	var identity [33]byte
	copy(identity[:], identityKey.SerializeCompressed())

	invoices := newLoopOutTestInvoices(t)
	wallet := &loopOutTestWallet{
		attempts: make(chan *wire.MsgTx, 4),
		sent:     make(chan *wire.MsgTx, 2),
	}
	notifier := &loopOutTestNotifier{
		registrations:      make(chan *loopOutTestConfRegistration, 2),
		spendRegistrations: make(chan *loopOutTestSpendRegistration, 2),
	}
	signer := &loopOutTestSigner{
		signed: make(chan [32]byte, 2),
	}
	lnd := &lndclient.LndServices{
		Client: &loopOutTestLightning{
			height: loopOutTestHeight,
			pubKey: identity,
		},
		WalletKit:     wallet,
		ChainNotifier: notifier,
		Signer:        signer,
		Invoices:      invoices,
		ChainParams:   &chaincfg.RegressionNetParams,
		NodePubkey:    identity,
	}
	server, err := New(context.Background(), Config{
		Lnd:     lnd,
		Bitcoin: loopOutTestBitcoin{},
	})
	require.NoError(t, err)

	return &loopOutTestHarness{
		server:   server,
		invoices: invoices,
		wallet:   wallet,
		notifier: notifier,
		signer:   signer,
	}
}

func loopOutTestRequest(t *testing.T) (*swapserverrpc.ServerLoopOutRequest,
	lntypes.Preimage) {

	t.Helper()
	var preimage lntypes.Preimage
	preimage[0] = 42
	hash := preimage.Hash()
	_, receiverPubKey := btcec.PrivKeyFromBytes([]byte{7})

	return &swapserverrpc.ServerLoopOutRequest{
		ReceiverKey:             receiverPubKey.SerializeCompressed(),
		SwapHash:                hash[:],
		Amt:                     500_000,
		SwapPublicationDeadline: time.Now().Add(time.Minute).Unix(),
		ProtocolVersion:         swapserverrpc.ProtocolVersion_MUSIG2,
		Expiry:                  loopOutTestHeight + 40,
		UserAgent:               "loopd/test",
	}, preimage
}

func receiveWithTimeout[T any](t *testing.T, channel <-chan T) T {
	t.Helper()

	select {
	case value := <-channel:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for test event")
		var zero T
		return zero
	}
}

func TestLoopOutFullHappyPathAndMuSig2(t *testing.T) {
	harness := newLoopOutTestHarness(t)
	defer harness.server.Stop()

	terms, err := harness.server.LoopOutTerms(
		context.Background(), &swapserverrpc.ServerLoopOutTermsRequest{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
		},
	)
	require.NoError(t, err)
	require.Equal(t, uint64(defaultMinSwapAmount), terms.MinSwapAmount)
	require.Equal(t, uint64(defaultMaxSwapAmount), terms.MaxSwapAmount)

	quote, err := harness.server.LoopOutQuote(
		context.Background(), &swapserverrpc.ServerLoopOutQuoteRequest{
			Amt:             500_000,
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
			Expiry:          loopOutTestHeight + 40,
		},
	)
	require.NoError(t, err)
	require.Equal(t, int64(600), quote.SwapFee)
	require.Len(t, quote.SwapPaymentDest, 66)

	request, preimage := loopOutTestRequest(t)
	response, err := harness.server.NewLoopOutSwap(
		context.Background(), request,
	)
	require.NoError(t, err)
	require.Len(t, response.SenderKey, 33)
	require.Equal(t, 2, harness.invoices.addCount())

	mainInvoice, err := zpay32.Decode(
		response.SwapInvoice, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.Equal(t, [32]byte(preimage.Hash()), *mainInvoice.PaymentHash)
	require.Equal(t, int64(500_500_000), int64(*mainInvoice.MilliSat))
	prepayInvoice, err := zpay32.Decode(
		response.PrepayInvoice, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.Equal(t, int64(100_000), int64(*prepayInvoice.MilliSat))

	duplicate, err := harness.server.NewLoopOutSwap(
		context.Background(), request,
	)
	require.NoError(t, err)
	require.Equal(t, response, duplicate)
	require.Equal(t, 2, harness.invoices.addCount())

	hash := preimage.Hash()
	resume, err := harness.server.NewLoopOutSwap(
		context.Background(), &swapserverrpc.ServerLoopOutRequest{
			SwapHash:  hash[:],
			UserAgent: "resume_swap",
		},
	)
	require.NoError(t, err)
	require.Equal(t, response, resume)
	require.Equal(t, 2, harness.invoices.addCount())

	subscribed := map[lntypes.Hash]bool{}
	for range 2 {
		subscribed[receiveWithTimeout(t, harness.invoices.subscribed)] = true
	}
	require.True(t, subscribed[hash])
	require.True(t, subscribed[*prepayInvoice.PaymentHash])

	// A preimage cannot settle the main invoice before the exact HTLC is
	// confirmed.
	_, err = harness.server.LoopOutPushPreimage(
		context.Background(), &swapserverrpc.ServerLoopOutPushPreimageRequest{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
			Preimage:        preimage[:],
		},
	)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	harness.invoices.sendState(t, hash, invpkg.ContractAccepted)
	select {
	case <-harness.wallet.sent:
		t.Fatal("HTLC published before prepay invoice was accepted")
	case <-time.After(50 * time.Millisecond):
	}
	harness.invoices.sendState(
		t, *prepayInvoice.PaymentHash, invpkg.ContractAccepted,
	)

	fundingTx := receiveWithTimeout(t, harness.wallet.sent)
	require.Len(t, fundingTx.TxOut, 1)
	require.Equal(t, int64(500_000), fundingTx.TxOut[0].Value)

	loopOut := harness.server.lookupLoopOut(hash)
	require.NotNil(t, loopOut)
	require.Equal(t, loopOut.htlc.PkScript, fundingTx.TxOut[0].PkScript)

	confirmation := receiveWithTimeout(t, harness.notifier.registrations)
	require.Equal(t, fundingTx.TxHash(), *confirmation.txid)
	require.Equal(t, int32(1), confirmation.confirmations)
	require.Equal(t, loopOutTestHeight, confirmation.heightHint)
	confirmation.confirmed <- &chainntnfs.TxConfirmation{
		Tx:          fundingTx,
		BlockHeight: uint32(loopOutTestHeight + 1),
	}

	prepaySettle := receiveWithTimeout(t, harness.invoices.settles)
	settledPrepayHash := prepaySettle.Hash()
	require.Equal(t, prepayInvoice.PaymentHash[:], settledPrepayHash[:])

	_, err = harness.server.LoopOutPushPreimage(
		context.Background(), &swapserverrpc.ServerLoopOutPushPreimageRequest{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
			Preimage:        preimage[:],
		},
	)
	require.NoError(t, err)
	require.Equal(t, preimage, receiveWithTimeout(t, harness.invoices.settles))

	// A duplicate preimage push is an idempotent acknowledgement.
	_, err = harness.server.LoopOutPushPreimage(
		context.Background(), &swapserverrpc.ServerLoopOutPushPreimageRequest{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
			Preimage:        preimage[:],
		},
	)
	require.NoError(t, err)

	loopOut.mu.Lock()
	fundingOutpoint := *loopOut.fundingOutpoint
	loopOut.mu.Unlock()

	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{PreviousOutPoint: fundingOutpoint})
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    499_000,
		PkScript: []byte{0x51},
	})
	packet, err := psbt.NewFromUnsignedTx(sweepTx)
	require.NoError(t, err)
	packet.Inputs[0].WitnessUtxo = &wire.TxOut{
		Value:    500_000,
		PkScript: bytes.Clone(loopOut.htlc.PkScript),
	}
	var packetBytes bytes.Buffer
	require.NoError(t, packet.Serialize(&packetBytes))

	paymentAddr, err := mainInvoice.PaymentAddr.UnwrapOrErr(
		context.Canceled,
	)
	require.NoError(t, err)
	clientNonce := make([]byte, musig2.PubNonceSize)
	clientNonce[0] = 3

	badPacket, err := psbt.NewFromUnsignedTx(sweepTx)
	require.NoError(t, err)
	badPacket.Inputs[0].WitnessUtxo = &wire.TxOut{
		Value:    500_000,
		PkScript: []byte{0x51},
	}
	var badPacketBytes bytes.Buffer
	require.NoError(t, badPacket.Serialize(&badPacketBytes))
	_, err = harness.server.MuSig2SignSweep(
		context.Background(), &swapserverrpc.MuSig2SignSweepReq{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
			SwapHash:        hash[:],
			PaymentAddress:  paymentAddr[:],
			Nonce:           clientNonce,
			SweepTxPsbt:     badPacketBytes.Bytes(),
		},
	)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Zero(t, harness.signer.createCount())

	signature, err := harness.server.MuSig2SignSweep(
		context.Background(), &swapserverrpc.MuSig2SignSweepReq{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
			SwapHash:        hash[:],
			PaymentAddress:  paymentAddr[:],
			Nonce:           clientNonce,
			SweepTxPsbt:     packetBytes.Bytes(),
		},
	)
	require.NoError(t, err)
	require.Len(t, signature.Nonce, musig2.PubNonceSize)
	require.Len(t, signature.PartialSignature, input.MuSig2PartialSigSize)
	require.NotEqual(t, [32]byte{}, receiveWithTimeout(t, harness.signer.signed))
	require.Equal(t, 1, harness.signer.createCount())

	subscription := loopOut.updates.subscribe()
	defer subscription.cancel()
	require.True(t, subscription.done)
	require.Equal(t, []swapserverrpc.ServerSwapState{
		swapserverrpc.ServerSwapState_SERVER_INITIATED,
		swapserverrpc.ServerSwapState_SERVER_HTLC_PUBLISHED,
		swapserverrpc.ServerSwapState_SERVER_HTLC_CONFIRMED,
		swapserverrpc.ServerSwapState_SERVER_SUCCESS,
	}, loopOutUpdateStates(subscription.history))
}

func TestLoopOutAmbiguousBroadcastReconciles(t *testing.T) {
	harness := newLoopOutTestHarness(t)
	defer harness.server.Stop()
	harness.wallet.failSend(errors.New("response lost after broadcast"))

	request, preimage := loopOutTestRequest(t)
	response, err := harness.server.NewLoopOutSwap(
		context.Background(), request,
	)
	require.NoError(t, err)

	prepayInvoice, err := zpay32.Decode(
		response.PrepayInvoice, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	hash := preimage.Hash()
	for range 2 {
		receiveWithTimeout(t, harness.invoices.subscribed)
	}
	harness.invoices.sendState(t, hash, invpkg.ContractAccepted)
	harness.invoices.sendState(
		t, *prepayInvoice.PaymentHash, invpkg.ContractAccepted,
	)

	// SendOutputs returned an error, but the transaction may already have
	// been broadcast. The server must retain both accepted invoices and use
	// a script-only confirmation to reconcile the ambiguous result.
	fundingTx := receiveWithTimeout(t, harness.wallet.attempts)
	confirmation := receiveWithTimeout(t, harness.notifier.registrations)
	require.Nil(t, confirmation.txid)
	select {
	case canceled := <-harness.invoices.cancels:
		t.Fatalf("invoice %x canceled after ambiguous broadcast", canceled)
	case <-time.After(50 * time.Millisecond):
	}

	confirmation.confirmed <- &chainntnfs.TxConfirmation{
		Tx:          fundingTx,
		BlockHeight: uint32(loopOutTestHeight + 1),
	}
	prepaySettle := receiveWithTimeout(t, harness.invoices.settles)
	settledPrepayHash := prepaySettle.Hash()
	require.Equal(t, prepayInvoice.PaymentHash[:], settledPrepayHash[:])

	_, err = harness.server.LoopOutPushPreimage(
		context.Background(), &swapserverrpc.ServerLoopOutPushPreimageRequest{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
			Preimage:        preimage[:],
		},
	)
	require.NoError(t, err)
	require.Equal(t, preimage, receiveWithTimeout(t, harness.invoices.settles))
	require.Zero(t, harness.invoices.cancelCount(hash))
	require.Zero(t, harness.invoices.cancelCount(*prepayInvoice.PaymentHash))

	loopOut := harness.server.lookupLoopOut(hash)
	subscription := loopOut.updates.subscribe()
	defer subscription.cancel()
	require.True(t, subscription.done)
	require.Equal(t, []swapserverrpc.ServerSwapState{
		swapserverrpc.ServerSwapState_SERVER_INITIATED,
		swapserverrpc.ServerSwapState_SERVER_HTLC_PUBLISHED,
		swapserverrpc.ServerSwapState_SERVER_HTLC_CONFIRMED,
		swapserverrpc.ServerSwapState_SERVER_SUCCESS,
	}, loopOutUpdateStates(subscription.history))
}

func TestLoopOutRecoversPreimageFromSuccessSpend(t *testing.T) {
	harness := newLoopOutTestHarness(t)
	defer harness.server.Stop()

	request, preimage := loopOutTestRequest(t)
	response, err := harness.server.NewLoopOutSwap(
		context.Background(), request,
	)
	require.NoError(t, err)
	prepayInvoice, err := zpay32.Decode(
		response.PrepayInvoice, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	hash := preimage.Hash()
	for range 2 {
		receiveWithTimeout(t, harness.invoices.subscribed)
	}
	harness.invoices.sendState(t, hash, invpkg.ContractAccepted)
	harness.invoices.sendState(
		t, *prepayInvoice.PaymentHash, invpkg.ContractAccepted,
	)

	fundingTx := receiveWithTimeout(t, harness.wallet.sent)
	loopOut := harness.server.lookupLoopOut(hash)
	confirmation := receiveWithTimeout(t, harness.notifier.registrations)
	confirmation.confirmed <- &chainntnfs.TxConfirmation{
		Tx:          fundingTx,
		BlockHeight: uint32(loopOutTestHeight + 1),
	}
	prepaySettle := receiveWithTimeout(t, harness.invoices.settles)
	settledPrepayHash := prepaySettle.Hash()
	require.Equal(t, prepayInvoice.PaymentHash[:], settledPrepayHash[:])

	spendRegistration := receiveWithTimeout(
		t, harness.notifier.spendRegistrations,
	)
	require.NotNil(t, spendRegistration.outpoint)
	require.Equal(t, loopOutTestHeight, spendRegistration.heightHint)
	require.Equal(t, loopOut.htlc.PkScript, spendRegistration.pkScript)

	sweepTx := loopOutTestSuccessSweep(t, loopOut, preimage)
	spendHash := sweepTx.TxHash()
	spend := &chainntnfs.SpendDetail{
		SpentOutPoint:     spendRegistration.outpoint,
		SpenderTxHash:     &spendHash,
		SpendingTx:        sweepTx,
		SpenderInputIndex: 0,
		SpendingHeight:    loopOutTestHeight + 2,
	}

	// A matching preimage alone isn't enough: the revealed script and its
	// complete witness must spend the exact committed success path.
	wrongScriptSpend := *spend
	wrongScriptSpend.SpendingTx = sweepTx.Copy()
	wrongScriptSpend.SpendingTx.TxIn[0].Witness[2] = []byte{txscript.OP_TRUE}
	_, err = harness.server.validateLoopOutSuccessSpend(
		loopOut, &wrongScriptSpend,
	)
	require.Error(t, err)

	wrongPreimageSpend := *spend
	wrongPreimageSpend.SpendingTx = sweepTx.Copy()
	wrongPreimageSpend.SpendingTx.TxIn[0].Witness[0] = make([]byte, 32)
	_, err = harness.server.validateLoopOutSuccessSpend(
		loopOut, &wrongPreimageSpend,
	)
	require.Error(t, err)

	// Never call LoopOutPushPreimage. The valid unilateral spend is the only
	// delivery mechanism for the main invoice preimage.
	spendRegistration.spends <- spend
	require.Equal(t, preimage, receiveWithTimeout(t, harness.invoices.settles))
	assertLoopOutTerminalState(
		t, loopOut, swapserverrpc.ServerSwapState_SERVER_SUCCESS,
	)

	// A late best-effort push observes the already completed result without
	// attempting another invoice settlement.
	_, err = harness.server.LoopOutPushPreimage(
		context.Background(), &swapserverrpc.ServerLoopOutPushPreimageRequest{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
			Preimage:        preimage[:],
		},
	)
	require.NoError(t, err)
	select {
	case duplicate := <-harness.invoices.settles:
		t.Fatalf("duplicate settlement with preimage %x", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLoopOutCancellationRetriesPartialAcknowledgement(t *testing.T) {
	harness := newLoopOutTestHarness(t)
	defer harness.server.Stop()

	request, preimage := loopOutTestRequest(t)
	response, err := harness.server.NewLoopOutSwap(
		context.Background(), request,
	)
	require.NoError(t, err)
	mainInvoice, err := zpay32.Decode(
		response.SwapInvoice, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	paymentAddr, err := mainInvoice.PaymentAddr.UnwrapOrErr(context.Canceled)
	require.NoError(t, err)
	prepayInvoice, err := zpay32.Decode(
		response.PrepayInvoice, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	hash := preimage.Hash()
	prepayHash := lntypes.Hash(*prepayInvoice.PaymentHash)
	harness.invoices.setCancelErrors(
		prepayHash, errors.New("temporary cancellation failure"),
	)
	cancelRequest := &swapserverrpc.CancelLoopOutSwapRequest{
		ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
		SwapHash:        hash[:],
		PaymentAddress:  paymentAddr[:],
		CancelInfo: &swapserverrpc.CancelLoopOutSwapRequest_RouteCancel{
			RouteCancel: &swapserverrpc.RouteCancel{
				RouteType: swapserverrpc.RoutePaymentType_INVOICE_ROUTE,
			},
		},
	}

	_, err = harness.server.CancelLoopOutSwap(
		context.Background(), cancelRequest,
	)
	require.Equal(t, codes.Unavailable, status.Code(err))
	firstCalls := map[lntypes.Hash]bool{}
	for range 2 {
		firstCalls[receiveWithTimeout(t, harness.invoices.cancels)] = true
	}
	require.True(t, firstCalls[hash])
	require.True(t, firstCalls[prepayHash])

	loopOut := harness.server.lookupLoopOut(hash)
	loopOut.mu.Lock()
	require.True(t, loopOut.cancelRequested)
	require.True(t, loopOut.mainCancelAck)
	require.False(t, loopOut.prepayCancelAck)
	require.False(t, loopOut.canceled)
	require.False(t, loopOut.terminal)
	loopOut.mu.Unlock()

	_, err = harness.server.CancelLoopOutSwap(
		context.Background(), cancelRequest,
	)
	require.NoError(t, err)
	require.Equal(t, prepayHash, receiveWithTimeout(t, harness.invoices.cancels))
	require.Equal(t, 1, harness.invoices.cancelCount(hash))
	require.Equal(t, 2, harness.invoices.cancelCount(prepayHash))

	subscription := loopOut.updates.subscribe()
	defer subscription.cancel()
	require.True(t, subscription.done)
	require.Equal(t,
		swapserverrpc.ServerSwapState_SERVER_CLIENT_INVOICE_CANCEL,
		subscription.history[len(subscription.history)-1].state,
	)
}

func TestLoopOutPublicationDeadline(t *testing.T) {
	fastDeadlines := map[string]int64{
		"cli-now":   time.Now().Unix(),
		"zero-time": time.Time{}.Unix(),
	}
	for name, deadline := range fastDeadlines {
		t.Run(name, func(t *testing.T) {
			harness := newLoopOutTestHarness(t)
			defer harness.server.Stop()

			request, preimage := loopOutTestRequest(t)
			request.SwapPublicationDeadline = deadline
			response, err := harness.server.NewLoopOutSwap(
				context.Background(), request,
			)
			require.NoError(t, err)
			prepayInvoice, err := zpay32.Decode(
				response.PrepayInvoice, &chaincfg.RegressionNetParams,
			)
			require.NoError(t, err)
			for range 2 {
				receiveWithTimeout(t, harness.invoices.subscribed)
			}

			loopOut := harness.server.lookupLoopOut(preimage.Hash())
			require.True(t, loopOut.publicationDeadline.IsZero())
			select {
			case canceled := <-harness.invoices.cancels:
				t.Fatalf("fast swap invoice %x canceled", canceled)
			case <-time.After(25 * time.Millisecond):
			}

			harness.invoices.sendState(
				t, preimage.Hash(), invpkg.ContractAccepted,
			)
			harness.invoices.sendState(
				t, *prepayInvoice.PaymentHash,
				invpkg.ContractAccepted,
			)
			receiveWithTimeout(t, harness.wallet.sent)
		})
	}

	t.Run("expires-while-waiting-for-invoices", func(t *testing.T) {
		harness := newLoopOutTestHarness(t)
		defer harness.server.Stop()

		request, preimage := loopOutTestRequest(t)
		request.SwapPublicationDeadline = time.Now().Add(2 * time.Second).Unix()
		response, err := harness.server.NewLoopOutSwap(
			context.Background(), request,
		)
		require.NoError(t, err)
		prepayInvoice, err := zpay32.Decode(
			response.PrepayInvoice, &chaincfg.RegressionNetParams,
		)
		require.NoError(t, err)
		for range 2 {
			receiveWithTimeout(t, harness.invoices.subscribed)
		}

		canceled := map[lntypes.Hash]bool{}
		for range 2 {
			canceled[receiveWithTimeout(t, harness.invoices.cancels)] = true
		}
		require.True(t, canceled[preimage.Hash()])
		require.True(t, canceled[*prepayInvoice.PaymentHash])
		select {
		case <-harness.wallet.attempts:
			t.Fatal("funding attempted after invoice-wait deadline")
		default:
		}
		assertLoopOutTerminalState(
			t, harness.server.lookupLoopOut(preimage.Hash()),
			swapserverrpc.ServerSwapState_SERVER_FAILED_HTLC_PUBLICATION,
		)
	})

	t.Run("expires-during-fee-estimation", func(t *testing.T) {
		harness := newLoopOutTestHarness(t)
		defer harness.server.Stop()
		harness.wallet.waitForEstimateDeadline()

		request, preimage := loopOutTestRequest(t)
		request.SwapPublicationDeadline = time.Now().Add(2 * time.Second).Unix()
		response, err := harness.server.NewLoopOutSwap(
			context.Background(), request,
		)
		require.NoError(t, err)
		prepayInvoice, err := zpay32.Decode(
			response.PrepayInvoice, &chaincfg.RegressionNetParams,
		)
		require.NoError(t, err)
		for range 2 {
			receiveWithTimeout(t, harness.invoices.subscribed)
		}
		harness.invoices.sendState(
			t, preimage.Hash(), invpkg.ContractAccepted,
		)
		harness.invoices.sendState(
			t, *prepayInvoice.PaymentHash, invpkg.ContractAccepted,
		)

		for range 2 {
			receiveWithTimeout(t, harness.invoices.cancels)
		}
		select {
		case <-harness.wallet.attempts:
			t.Fatal("SendOutputs called after fee-estimation deadline")
		default:
		}
		assertLoopOutTerminalState(
			t, harness.server.lookupLoopOut(preimage.Hash()),
			swapserverrpc.ServerSwapState_SERVER_FAILED_HTLC_PUBLICATION,
		)
	})
}

func assertLoopOutTerminalState(t *testing.T, loopOut *loopOutSwap,
	want swapserverrpc.ServerSwapState) {

	t.Helper()
	require.Eventually(t, func() bool {
		subscription := loopOut.updates.subscribe()
		defer subscription.cancel()
		if !subscription.done || len(subscription.history) == 0 {
			return false
		}

		return subscription.history[len(subscription.history)-1].state == want
	}, time.Second, 10*time.Millisecond)
	subscription := loopOut.updates.subscribe()
	defer subscription.cancel()
	require.True(t, subscription.done)
	require.Equal(t, want,
		subscription.history[len(subscription.history)-1].state,
	)
}

func TestLoopOutCancelOwnershipAndGating(t *testing.T) {
	harness := newLoopOutTestHarness(t)
	defer harness.server.Stop()

	request, preimage := loopOutTestRequest(t)
	response, err := harness.server.NewLoopOutSwap(
		context.Background(), request,
	)
	require.NoError(t, err)

	mainInvoice, err := zpay32.Decode(
		response.SwapInvoice, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	paymentAddr, err := mainInvoice.PaymentAddr.UnwrapOrErr(
		context.Canceled,
	)
	require.NoError(t, err)

	hash := preimage.Hash()
	badPaymentAddr := paymentAddr
	badPaymentAddr[0] ^= 1
	_, err = harness.server.CancelLoopOutSwap(
		context.Background(), &swapserverrpc.CancelLoopOutSwapRequest{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
			SwapHash:        hash[:],
			PaymentAddress:  badPaymentAddr[:],
			CancelInfo: &swapserverrpc.CancelLoopOutSwapRequest_RouteCancel{
				RouteCancel: &swapserverrpc.RouteCancel{
					RouteType: swapserverrpc.RoutePaymentType_INVOICE_ROUTE,
				},
			},
		},
	)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	_, err = harness.server.CancelLoopOutSwap(
		context.Background(), &swapserverrpc.CancelLoopOutSwapRequest{
			ProtocolVersion: swapserverrpc.ProtocolVersion_MUSIG2,
			SwapHash:        hash[:],
			PaymentAddress:  paymentAddr[:],
			CancelInfo: &swapserverrpc.CancelLoopOutSwapRequest_RouteCancel{
				RouteCancel: &swapserverrpc.RouteCancel{
					RouteType: swapserverrpc.RoutePaymentType_INVOICE_ROUTE,
				},
			},
		},
	)
	require.NoError(t, err)

	canceled := map[lntypes.Hash]bool{}
	for range 2 {
		canceled[receiveWithTimeout(t, harness.invoices.cancels)] = true
	}
	require.True(t, canceled[hash])
	prepayInvoice, err := zpay32.Decode(
		response.PrepayInvoice, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	require.True(t, canceled[*prepayInvoice.PaymentHash])

	loopOut := harness.server.lookupLoopOut(hash)
	subscription := loopOut.updates.subscribe()
	defer subscription.cancel()
	require.True(t, subscription.done)
	require.Equal(t,
		swapserverrpc.ServerSwapState_SERVER_CLIENT_INVOICE_CANCEL,
		subscription.history[len(subscription.history)-1].state,
	)
}

func loopOutUpdateStates(updates []serverUpdate) []swapserverrpc.ServerSwapState {
	states := make([]swapserverrpc.ServerSwapState, len(updates))
	for index := range updates {
		states[index] = updates[index].state
	}

	return states
}

func loopOutTestSuccessSweep(t *testing.T, loopOut *loopOutSwap,
	preimage lntypes.Preimage) *wire.MsgTx {

	t.Helper()
	loopOut.mu.Lock()
	require.NotNil(t, loopOut.fundingOutpoint)
	outpoint := *loopOut.fundingOutpoint
	amount := loopOut.amount
	htlc := loopOut.htlc
	loopOut.mu.Unlock()

	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: outpoint,
		Sequence:         htlc.SuccessSequence(),
	})
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    int64(amount - 1_000),
		PkScript: []byte{txscript.OP_TRUE},
	})
	prevOutFetcher := txscript.NewCannedPrevOutputFetcher(
		htlc.PkScript, int64(amount),
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevOutFetcher)
	htlcV3, ok := htlc.HtlcScript.(*swap.HtlcScriptV3)
	require.True(t, ok)
	receiverPrivateKey, _ := btcec.PrivKeyFromBytes([]byte{7})
	signature, err := txscript.RawTxInTapscriptSignature(
		sweepTx, sigHashes, 0, int64(amount), htlc.PkScript,
		txscript.NewBaseTapLeaf(htlcV3.SuccessScript()), htlc.SigHash(),
		receiverPrivateKey,
	)
	require.NoError(t, err)
	sweepTx.TxIn[0].Witness, err = htlc.GenSuccessWitness(
		signature, preimage,
	)
	require.NoError(t, err)

	return sweepTx
}
