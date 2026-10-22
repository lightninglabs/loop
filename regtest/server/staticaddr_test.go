package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testMuSig2Signer adapts lnd's real in-memory MuSig2 session manager to the
// lndclient interface. Embedding the interface supplies methods irrelevant to
// this focused test; every MuSig2 method exercised by the server is real.
type testMuSig2Signer struct {
	lndclient.SignerClient

	manager *input.MusigSessionManager
	rawKeys []*btcec.PrivateKey
}

type testStaticWallet struct {
	lndclient.WalletKitClient

	address     btcutil.Address
	feeRate     chainfee.SatPerKWeight
	derivedKey  *keychain.KeyDescriptor
	deriveCalls int
}

// testStaticBitcoin provides an exact raw transaction and a mutable UTXO view
// so tests can model a deposit being spent between admission and payment.
type testStaticBitcoin struct {
	mu sync.Mutex

	tx            *wire.MsgTx
	confirmations int64
	spent         bool
}

func (b *testStaticBitcoin) GetTxOut(hash *chainhash.Hash, index uint32,
	_ bool) (*btcjson.GetTxOutResult, error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.spent || hash == nil || b.tx == nil || b.tx.TxHash() != *hash ||
		int(index) >= len(b.tx.TxOut) {

		return nil, nil
	}

	return &btcjson.GetTxOutResult{
		Confirmations: b.confirmations,
		ScriptPubKey: btcjson.ScriptPubKeyResult{
			Hex: fmt.Sprintf("%x", b.tx.TxOut[index].PkScript),
		},
	}, nil
}

func (b *testStaticBitcoin) GetRawTransaction(
	hash *chainhash.Hash) (*btcutil.Tx, error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	if hash == nil || b.tx == nil || b.tx.TxHash() != *hash {
		return nil, fmt.Errorf("transaction %v not found", hash)
	}

	return btcutil.NewTx(b.tx.Copy()), nil
}

func (b *testStaticBitcoin) setSpent(spent bool) {
	b.mu.Lock()
	b.spent = spent
	b.mu.Unlock()
}

type testStaticLightning struct {
	lndclient.LightningClient

	height uint32
}

func (l *testStaticLightning) GetInfo(context.Context) (*lndclient.Info,
	error) {

	return &lndclient.Info{BlockHeight: l.height}, nil
}

type testStaticRouter struct {
	lndclient.RouterClient

	mu        sync.Mutex
	sendCalls int
}

func (r *testStaticRouter) SendPayment(context.Context,
	lndclient.SendPaymentRequest) (chan lndclient.PaymentStatus, chan error,
	error) {

	r.mu.Lock()
	r.sendCalls++
	r.mu.Unlock()

	return nil, nil, fmt.Errorf("unexpected payment")
}

func (r *testStaticRouter) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.sendCalls
}

func encodeStaticTestInvoice(t *testing.T, signer *btcec.PrivateKey,
	hash lntypes.Hash, amount btcutil.Amount) string {

	t.Helper()

	invoice, err := zpay32.NewInvoice(
		&chaincfg.RegressionNetParams, hash, time.Unix(1_700_000_000, 0),
		zpay32.Description("regtest static Loop In"),
		zpay32.Amount(lnwire.NewMSatFromSatoshis(amount)),
	)
	require.NoError(t, err)

	encoded, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(message []byte) ([]byte, error) {
			digest := chainhash.HashB(message)

			return ecdsa.SignCompact(signer, digest, true), nil
		},
	})
	require.NoError(t, err)

	return encoded
}

func (w *testStaticWallet) DeriveNextKey(context.Context, int32) (
	*keychain.KeyDescriptor, error) {

	w.deriveCalls++

	return w.derivedKey, nil
}

func (w *testStaticWallet) NextAddr(context.Context, string,
	walletrpc.AddressType, bool) (btcutil.Address, error) {

	return w.address, nil
}

func (w *testStaticWallet) EstimateFeeRate(context.Context,
	int32) (chainfee.SatPerKWeight, error) {

	return w.feeRate, nil
}

func newTestMuSig2Signer(privateKey *btcec.PrivateKey,
	locator keychain.KeyLocator) *testMuSig2Signer {

	keyFetcher := func(keyDesc *keychain.KeyDescriptor) (
		*btcec.PrivateKey, error) {

		if keyDesc.KeyLocator != locator {
			return nil, fmt.Errorf("unexpected key locator: %v",
				keyDesc.KeyLocator)
		}

		return privateKey, nil
	}

	return &testMuSig2Signer{
		manager: input.NewMusigSessionManager(keyFetcher),
		rawKeys: []*btcec.PrivateKey{privateKey},
	}
}

func (s *testMuSig2Signer) SignOutputRaw(_ context.Context, tx *wire.MsgTx,
	descriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([][]byte, error) {

	return s.signOutputRaw(tx, descriptors, prevOutputs)
}

func (s *testMuSig2Signer) SignOutputRawKeyLocator(_ context.Context,
	tx *wire.MsgTx, descriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([][]byte, error) {

	return s.signOutputRaw(tx, descriptors, prevOutputs)
}

func (s *testMuSig2Signer) signOutputRaw(tx *wire.MsgTx,
	descriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([][]byte, error) {

	if len(prevOutputs) != len(tx.TxIn) {
		return nil, fmt.Errorf("got %d prevouts for %d inputs",
			len(prevOutputs), len(tx.TxIn))
	}
	prevFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, txIn := range tx.TxIn {
		prevFetcher.AddPrevOut(txIn.PreviousOutPoint, prevOutputs[i])
	}
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	signatures := make([][]byte, len(descriptors))
	for i, descriptor := range descriptors {
		if descriptor.KeyDesc.PubKey == nil {
			return nil, fmt.Errorf("descriptor %d has no public key", i)
		}
		var privateKey *btcec.PrivateKey
		for _, candidate := range s.rawKeys {
			if candidate.PubKey().IsEqual(descriptor.KeyDesc.PubKey) {
				privateKey = candidate
				break
			}
		}
		if privateKey == nil {
			return nil, fmt.Errorf("signing key %x not found",
				descriptor.KeyDesc.PubKey.SerializeCompressed())
		}

		signer := input.NewMockSigner(
			[]*btcec.PrivateKey{privateKey},
			&chaincfg.RegressionNetParams,
		)
		signature, err := signer.SignOutputRaw(tx, &input.SignDescriptor{
			KeyDesc:           descriptor.KeyDesc,
			SingleTweak:       descriptor.SingleTweak,
			DoubleTweak:       descriptor.DoubleTweak,
			TapTweak:          descriptor.TapTweak,
			WitnessScript:     descriptor.WitnessScript,
			SignMethod:        descriptor.SignMethod,
			Output:            descriptor.Output,
			HashType:          descriptor.HashType,
			SigHashes:         sigHashes,
			PrevOutputFetcher: prevFetcher,
			InputIndex:        descriptor.InputIndex,
		})
		if err != nil {
			return nil, err
		}
		signatures[i] = signature.Serialize()
	}

	return signatures, nil
}

func (s *testMuSig2Signer) MuSig2CreateSession(_ context.Context,
	version input.MuSig2Version, signerLoc *keychain.KeyLocator,
	signers [][]byte, opts ...lndclient.MuSig2SessionOpts) (
	*input.MuSig2SessionInfo, error) {

	parsedSigners, err := input.MuSig2ParsePubKeys(version, signers)
	if err != nil {
		return nil, err
	}

	request := &signrpc.MuSig2SessionRequest{}
	for _, opt := range opts {
		opt(request)
	}

	tweaks := &input.MuSig2Tweaks{}
	if request.TaprootTweak != nil {
		if request.TaprootTweak.KeySpendOnly {
			tweaks.TaprootBIP0086Tweak = true
		} else {
			tweaks.TaprootTweak = request.TaprootTweak.ScriptRoot
		}
	}

	nonces := make(
		[][musig2.PubNonceSize]byte,
		len(request.OtherSignerPublicNonces),
	)
	for i, rawNonce := range request.OtherSignerPublicNonces {
		if len(rawNonce) != musig2.PubNonceSize {
			return nil, fmt.Errorf("invalid nonce length: %d",
				len(rawNonce))
		}
		copy(nonces[i][:], rawNonce)
	}

	return s.manager.MuSig2CreateSession(
		version, *signerLoc, parsedSigners, tweaks, nonces, nil,
	)
}

func (s *testMuSig2Signer) MuSig2RegisterNonces(_ context.Context,
	sessionID [32]byte, nonces [][musig2.PubNonceSize]byte) (
	bool, error) {

	return s.manager.MuSig2RegisterNonces(
		input.MuSig2SessionID(sessionID), nonces,
	)
}

func (s *testMuSig2Signer) MuSig2Sign(_ context.Context,
	sessionID [32]byte, message [32]byte, cleanup bool) ([]byte, error) {

	partialSig, err := s.manager.MuSig2Sign(
		input.MuSig2SessionID(sessionID), message, cleanup,
	)
	if err != nil {
		return nil, err
	}

	serialized, err := input.SerializePartialSignature(partialSig)
	if err != nil {
		return nil, err
	}

	return serialized[:], nil
}

func (s *testMuSig2Signer) MuSig2CombineSig(_ context.Context,
	sessionID [32]byte, otherPartialSigs [][]byte) (bool, []byte, error) {

	partialSigs := make(
		[]*musig2.PartialSignature, len(otherPartialSigs),
	)
	for i, serialized := range otherPartialSigs {
		partialSig, err := input.DeserializePartialSignature(serialized)
		if err != nil {
			return false, nil, err
		}
		partialSigs[i] = partialSig
	}

	finalSig, haveAllSigs, err := s.manager.MuSig2CombineSig(
		input.MuSig2SessionID(sessionID), partialSigs,
	)
	if err != nil || finalSig == nil {
		return haveAllSigs, nil, err
	}

	return haveAllSigs, finalSig.Serialize(), nil
}

func (s *testMuSig2Signer) MuSig2Cleanup(_ context.Context,
	sessionID [32]byte) error {

	return s.manager.MuSig2Cleanup(input.MuSig2SessionID(sessionID))
}

func TestServerNewAddressIdempotent(t *testing.T) {
	t.Parallel()

	_, clientPubKey := btcec.PrivKeyFromBytes([]byte{11})
	_, serverPubKey := btcec.PrivKeyFromBytes([]byte{12})
	serverLocator := keychain.KeyLocator{Family: 80, Index: 3}
	wallet := &testStaticWallet{derivedKey: &keychain.KeyDescriptor{
		KeyLocator: serverLocator,
		PubKey:     serverPubKey,
	}}
	server := &Server{
		cfg: Config{
			Lnd: &lndclient.LndServices{
				WalletKit:   wallet,
				ChainParams: &chaincfg.RegressionNetParams,
			},
			StaticAddressExpiry: 4_320,
		},
		addresses: make(map[string]*staticAddress),
	}
	request := &swapserverrpc.ServerNewAddressRequest{
		ProtocolVersion: swapserverrpc.StaticAddressProtocolVersion_V0,
		ClientKey:       clientPubKey.SerializeCompressed(),
	}

	first, err := server.ServerNewAddress(t.Context(), request)
	require.NoError(t, err)
	second, err := server.ServerNewAddress(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, 1, wallet.deriveCalls)
	require.Equal(t, serverPubKey.SerializeCompressed(),
		first.Params.ServerKey)
	require.Equal(t, uint32(4_320), first.Params.Expiry)

	_, err = server.ServerNewAddress(t.Context(),
		&swapserverrpc.ServerNewAddressRequest{
			ProtocolVersion: swapserverrpc.StaticAddressProtocolVersion(1),
			ClientKey:       clientPubKey.SerializeCompressed(),
		},
	)
	require.ErrorContains(t, err, "unsupported static address protocol")
	require.Equal(t, 1, wallet.deriveCalls)
}

type staticAdmissionHarness struct {
	server        *Server
	request       *swapserverrpc.ServerStaticAddressLoopInRequest
	bitcoin       *testStaticBitcoin
	router        *testStaticRouter
	clientSigner  *testMuSig2Signer
	clientLocator keychain.KeyLocator
}

func newStaticAdmissionHarness(t *testing.T,
	confirmations int64) *staticAdmissionHarness {

	t.Helper()

	const (
		currentHeight = uint32(5_000)
		depositValue  = btcutil.Amount(500_000)
		addressExpiry = uint32(4_320)
	)

	clientAddressPriv, clientAddressPub := btcec.PrivKeyFromBytes(
		[]byte{21},
	)
	serverAddressPriv, serverAddressPub := btcec.PrivKeyFromBytes(
		[]byte{22},
	)
	_, htlcClientPub := btcec.PrivKeyFromBytes([]byte{23})
	invoicePriv, _ := btcec.PrivKeyFromBytes([]byte{24})
	clientLocator := keychain.KeyLocator{Family: 80, Index: 21}
	serverLocator := keychain.KeyLocator{Family: 80, Index: 22}

	contract, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(addressExpiry),
		clientAddressPub, serverAddressPub,
	)
	require.NoError(t, err)
	pkScript, err := contract.StaticAddressScript()
	require.NoError(t, err)

	fundingTx := wire.NewMsgTx(2)
	fundingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("static admission source")),
			Index: 1,
		},
	})
	fundingTx.AddTxOut(&wire.TxOut{
		Value:    int64(depositValue),
		PkScript: pkScript,
	})
	outpoint := wire.OutPoint{Hash: fundingTx.TxHash(), Index: 0}
	bitcoin := &testStaticBitcoin{
		tx:            fundingTx,
		confirmations: confirmations,
	}
	router := &testStaticRouter{}
	serverSigner := newTestMuSig2Signer(
		serverAddressPriv, serverLocator,
	)
	wallet := &testStaticWallet{derivedKey: &keychain.KeyDescriptor{
		KeyLocator: serverLocator,
		PubKey:     serverAddressPub,
	}}
	server := &Server{
		cfg: Config{
			Lnd: &lndclient.LndServices{
				Client:      &testStaticLightning{height: currentHeight},
				WalletKit:   wallet,
				Signer:      serverSigner,
				Router:      router,
				ChainParams: &chaincfg.RegressionNetParams,
			},
			Bitcoin:             bitcoin,
			MinSwapAmount:       50_000,
			MaxSwapAmount:       5_000_000,
			FeeBaseSat:          100,
			FeePPM:              1_000,
			StaticAddressExpiry: addressExpiry,
			PaymentTimeout:      time.Minute,
			Logger:              log.New(io.Discard, "", 0),
		},
		ctx:           context.Background(),
		staticSwaps:   make(map[lntypes.Hash]*staticLoopInSwap),
		addresses:     make(map[string]*staticAddress),
		lockedUTXOs:   make(map[string]lntypes.Hash),
		notifications: newNotificationHub(),
	}
	server.addresses[string(clientAddressPub.SerializeCompressed())] =
		&staticAddress{
			clientKey: clientAddressPub,
			serverKey: &serverKey{
				pubKey:  serverAddressPub,
				locator: serverLocator,
			},
			expiry:   addressExpiry,
			contract: contract,
			pkScript: pkScript,
		}

	var preimage lntypes.Preimage
	preimage[0] = 25
	hash := preimage.Hash()
	invoiceAmount := depositValue - server.swapFee(depositValue)
	request := &swapserverrpc.ServerStaticAddressLoopInRequest{
		ProtocolVersion:  swapserverrpc.StaticAddressProtocolVersion_V0,
		SwapHash:         hash[:],
		HtlcClientPubKey: htlcClientPub.SerializeCompressed(),
		SwapInvoice: encodeStaticTestInvoice(
			t, invoicePriv, hash, invoiceAmount,
		),
		DepositOutpoints: []string{outpoint.String()},
		DepositToClientPubkeys: map[string]*swapserverrpc.
			StaticAddressDescriptor{
			outpoint.String(): {
				Pubkey:   clientAddressPub.SerializeCompressed(),
				PkScript: pkScript,
			},
		},
	}

	return &staticAdmissionHarness{
		server:        server,
		request:       request,
		bitcoin:       bitcoin,
		router:        router,
		clientSigner:  newTestMuSig2Signer(clientAddressPriv, clientLocator),
		clientLocator: clientLocator,
	}
}

func TestStaticDepositAdmissionPolicy(t *testing.T) {
	t.Parallel()

	// At height 5,000 with a 4,320-block CSV, 3,271 confirmations
	// leave exactly 1,050 blocks. One additional confirmation makes the
	// deposit one block too old.
	testCases := []struct {
		name          string
		confirmations int64
		errorCode     codes.Code
		errorContains string
	}{
		{
			name:          "unconfirmed",
			confirmations: 0,
			errorCode:     codes.FailedPrecondition,
			errorContains: "unconfirmed",
		},
		{
			name:          "near expiry",
			confirmations: 3_272,
			errorCode:     codes.FailedPrecondition,
			errorContains: "residual CSV lifetime",
		},
		{
			name:          "exact lifetime boundary",
			confirmations: 3_271,
			errorCode:     codes.OK,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			harness := newStaticAdmissionHarness(
				t, testCase.confirmations,
			)
			response, err := harness.server.ServerStaticAddressLoopIn(
				t.Context(), harness.request,
			)
			if testCase.errorCode != codes.OK {
				require.Equal(t, testCase.errorCode, status.Code(err))
				require.ErrorContains(t, err, testCase.errorContains)
				require.Nil(t, response)

				return
			}

			require.NoError(t, err)
			require.NotNil(t, response)
			hash, err := parseHash(harness.request.SwapHash)
			require.NoError(t, err)
			staticSwap := harness.server.staticSwaps[hash]
			require.NotNil(t, staticSwap)
			require.EqualValues(
				t, staticDepositMinLifetime,
				int64(staticSwap.address.expiry)-testCase.confirmations+1,
			)
			harness.server.cleanupStaticSessions(
				context.Background(), staticSwap,
			)
			harness.server.releaseStaticLocks(staticSwap)
		})
	}
}

func TestStaticDepositSpentBeforePayment(t *testing.T) {
	t.Parallel()

	harness := newStaticAdmissionHarness(t, 1)
	_, err := harness.server.ServerStaticAddressLoopIn(
		t.Context(), harness.request,
	)
	require.NoError(t, err)
	hash, err := parseHash(harness.request.SwapHash)
	require.NoError(t, err)
	staticSwap := harness.server.staticSwaps[hash]
	require.NotNil(t, staticSwap)

	clientInfos := make(
		[]*swapserverrpc.ClientHtlcSigningInfo,
		len(staticFundingFeeRates),
	)
	for i, round := range staticSwap.fundingRounds {
		clientInfos[i] = signStaticFundingRound(
			t, t.Context(), harness.clientSigner,
			harness.clientLocator, staticSwap, round,
		)
	}

	notifications := harness.server.notifications.subscribe(t.Context())

	// Admission took a valid UTXO snapshot. Model a conflicting spend after
	// the backup signatures have been prepared but before the payment worker
	// is allowed to accept risk or dispatch the invoice.
	harness.bitcoin.setSpent(true)
	_, err = harness.server.PushStaticAddressHtlcSigs(
		t.Context(), &swapserverrpc.PushStaticAddressHtlcSigsRequest{
			SwapHash:           hash[:],
			StandardHtlcInfo:   clientInfos[0],
			HighFeeHtlcInfo:    clientInfos[1],
			ExtremeFeeHtlcInfo: clientInfos[2],
		},
	)
	require.NoError(t, err)
	harness.server.wg.Wait()

	select {
	case notification := <-notifications:
		rejected := notification.GetStaticLoopInRiskRejected()
		require.NotNil(t, rejected)
		require.Equal(t, hash[:], rejected.SwapHash)
		require.Nil(t, notification.GetStaticLoopInRiskAccepted())

	case <-time.After(time.Second):
		t.Fatal("risk rejection notification not received")
	}

	require.Zero(t, harness.router.calls())
	harness.server.mu.RLock()
	_, locked := harness.server.lockedUTXOs[staticSwap.depositStrings[0]]
	harness.server.mu.RUnlock()
	require.False(t, locked)
}

func TestStaticFundingRoundsFinalize(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clientAddressPriv, clientAddressPub := btcec.PrivKeyFromBytes(
		[]byte{1},
	)
	serverAddressPriv, serverAddressPub := btcec.PrivKeyFromBytes(
		[]byte{2},
	)
	_, clientHtlcPub := btcec.PrivKeyFromBytes([]byte{3})
	serverHtlcPriv, serverHtlcPub := btcec.PrivKeyFromBytes([]byte{4})
	clientLocator := keychain.KeyLocator{Family: 80, Index: 1}
	serverLocator := keychain.KeyLocator{Family: 80, Index: 2}

	contract, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, 4_320, clientAddressPub,
		serverAddressPub,
	)
	require.NoError(t, err)
	pkScript, err := contract.StaticAddressScript()
	require.NoError(t, err)

	var preimage lntypes.Preimage
	preimage[0] = 5
	htlc, err := swap.NewHtlcV2(
		1_000, keyBytes(clientHtlcPub), keyBytes(serverHtlcPub),
		preimage.Hash(), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	outpointOne := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("static-deposit-one")),
		Index: 0,
	}
	outpointTwo := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("static-deposit-two")),
		Index: 1,
	}
	prevOuts := map[wire.OutPoint]*wire.TxOut{
		outpointOne: {
			Value:    300_000,
			PkScript: pkScript,
		},
		outpointTwo: {
			Value:    250_000,
			PkScript: pkScript,
		},
	}

	serverSigner := newTestMuSig2Signer(
		serverAddressPriv, serverLocator,
	)
	serverSigner.rawKeys = append(serverSigner.rawKeys, serverHtlcPriv)
	clientSigner := newTestMuSig2Signer(
		clientAddressPriv, clientLocator,
	)
	directAddress, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(serverHtlcPub),
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	server := &Server{
		cfg: Config{Lnd: &lndclient.LndServices{
			Signer: serverSigner,
			WalletKit: &testStaticWallet{
				address: directAddress,
				feeRate: chainfee.SatPerKWeight(500),
			},
			ChainParams: &chaincfg.RegressionNetParams,
		}},
		staticSwaps: make(map[lntypes.Hash]*staticLoopInSwap),
		lockedUTXOs: make(map[string]lntypes.Hash),
	}
	staticSwap := &staticLoopInSwap{
		hash:           preimage.Hash(),
		deposits:       []wire.OutPoint{outpointOne, outpointTwo},
		prevOuts:       prevOuts,
		changePkScript: pkScript,
		address: &staticAddress{
			clientKey: clientAddressPub,
			serverKey: &serverKey{
				pubKey:  serverAddressPub,
				locator: serverLocator,
			},
			contract: contract,
			pkScript: pkScript,
		},
		totalDepositAmount: 550_000,
		swapAmount:         400_000,
		htlcClientKey:      clientHtlcPub,
		htlcServerKey: &serverKey{
			pubKey:  serverHtlcPub,
			locator: keychain.KeyLocator{Family: 80, Index: 4},
		},
		htlc:          htlc,
		workerStarted: true,
	}

	clientInfos := make(
		[]*swapserverrpc.ClientHtlcSigningInfo,
		len(staticFundingFeeRates),
	)
	for i, feeRate := range staticFundingFeeRates {
		round, err := server.newStaticFundingRound(
			ctx, staticSwap, feeRate,
		)
		require.NoError(t, err)
		staticSwap.fundingRounds[i] = round
		clientInfos[i] = signStaticFundingRound(
			t, ctx, clientSigner, clientLocator, staticSwap, round,
		)
	}

	server.staticSwaps[staticSwap.hash] = staticSwap
	request := &swapserverrpc.PushStaticAddressHtlcSigsRequest{
		SwapHash:           staticSwap.hash[:],
		StandardHtlcInfo:   clientInfos[0],
		HighFeeHtlcInfo:    clientInfos[1],
		ExtremeFeeHtlcInfo: clientInfos[2],
	}
	_, err = server.PushStaticAddressHtlcSigs(ctx, request)
	require.NoError(t, err)
	require.True(t, staticSwap.backupFinalized)

	txids := make(map[chainhash.Hash]struct{}, len(staticFundingFeeRates))
	for _, round := range staticSwap.fundingRounds {
		require.NotNil(t, round.finalTx)
		require.NoError(t, validateStaticFundingTx(
			round.finalTx, prevOuts,
		))
		require.Len(t, round.finalTx.TxIn, len(staticSwap.deposits))
		for _, txIn := range round.finalTx.TxIn {
			require.Len(t, txIn.Witness, 1)
			require.Len(t, txIn.Witness[0], 64)
		}
		txids[round.finalTx.TxHash()] = struct{}{}
	}
	require.Len(t, txids, len(staticFundingFeeRates))

	// A retry must not attempt to register a nonce or combine a signature
	// into an already consumed session.
	_, err = server.PushStaticAddressHtlcSigs(ctx, request)
	require.NoError(t, err)

	// The fallback claim is a real, script-validated P2WSH success spend.
	staticSwap.paymentPreimage = preimage
	successSweep, err := server.createStaticSuccessSweep(
		ctx, staticSwap, staticSwap.fundingRounds[0].finalTx,
	)
	require.NoError(t, err)
	require.Equal(t, htlc.SuccessSequence(), successSweep.TxIn[0].Sequence)
	require.True(t, htlc.IsSuccessWitness(successSweep.TxIn[0].Witness))

	directRound, err := server.newStaticSweeplessRound(ctx, staticSwap)
	require.NoError(t, err)
	staticSwap.sweepless = directRound
	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(directRound.psbt), false,
	)
	require.NoError(t, err)
	require.Len(t, packet.Inputs, len(staticSwap.deposits))
	for i, packetInput := range packet.Inputs {
		require.Equal(t, staticSwap.prevOuts[staticSwap.deposits[i]],
			packetInput.WitnessUtxo)
	}
	directInfos := signStaticSweeplessRound(
		t, ctx, clientSigner, clientLocator, staticSwap, directRound,
	)
	directTxID := directRound.tx.TxHash()
	directRequest := &swapserverrpc.PushStaticAddressSweeplessSigsRequest{
		SwapHash:    staticSwap.hash[:],
		Txid:        directTxID[:],
		SigningInfo: directInfos,
	}
	_, err = server.PushStaticAddressSweeplessSigs(
		ctx, &swapserverrpc.PushStaticAddressSweeplessSigsRequest{
			SwapHash:     staticSwap.hash[:],
			Txid:         directTxID[:],
			ErrorMessage: "swap not finished",
		},
	)
	require.NoError(t, err)
	_, err = server.PushStaticAddressSweeplessSigs(ctx, directRequest)
	require.NoError(t, err)
	require.NotNil(t, directRound.finalTx)
	require.NoError(t, validateStaticFundingTx(
		directRound.finalTx, staticSwap.prevOuts,
	))

	// The direct-signature submission is idempotent too.
	_, err = server.PushStaticAddressSweeplessSigs(ctx, directRequest)
	require.NoError(t, err)

	// Retrying the protocol's empty-signature abandonment signal is also
	// harmless if the first response was lost.
	abandonedHash := lntypes.Hash{9}
	server.staticSwaps[abandonedHash] = &staticLoopInSwap{abandoned: true}
	_, err = server.PushStaticAddressHtlcSigs(
		ctx, &swapserverrpc.PushStaticAddressHtlcSigsRequest{
			SwapHash: abandonedHash[:],
		},
	)
	require.NoError(t, err)
}

func signStaticFundingRound(t *testing.T, ctx context.Context,
	signer *testMuSig2Signer, locator keychain.KeyLocator,
	staticSwap *staticLoopInSwap,
	round *staticFundingRound) *swapserverrpc.ClientHtlcSigningInfo {

	t.Helper()

	signers := [][]byte{
		staticSwap.address.clientKey.SerializeCompressed(),
		staticSwap.address.serverKey.pubKey.SerializeCompressed(),
	}
	prevFetcher := txscript.NewMultiPrevOutFetcher(staticSwap.prevOuts)
	sigHashes := txscript.NewTxSigHashes(round.tx, prevFetcher)
	info := &swapserverrpc.ClientHtlcSigningInfo{
		Nonces: make([][]byte, len(staticSwap.deposits)),
		Sigs:   make([][]byte, len(staticSwap.deposits)),
	}

	for i := range staticSwap.deposits {
		session, err := signer.MuSig2CreateSession(
			ctx, input.MuSig2Version100RC2, &locator, signers,
			lndclient.MuSig2TaprootTweakOpt(
				staticSwap.address.contract.RootHash[:], false,
			),
		)
		require.NoError(t, err)
		info.Nonces[i] = append([]byte(nil), session.PublicNonce[:]...)

		haveAllNonces, err := signer.MuSig2RegisterNonces(
			ctx, session.SessionID,
			[][musig2.PubNonceSize]byte{
				round.sessions[i].PublicNonce,
			},
		)
		require.NoError(t, err)
		require.True(t, haveAllNonces)

		digestBytes, err := txscript.CalcTaprootSignatureHash(
			sigHashes, txscript.SigHashDefault, round.tx, i,
			prevFetcher,
		)
		require.NoError(t, err)
		var digest [32]byte
		copy(digest[:], digestBytes)
		info.Sigs[i], err = signer.MuSig2Sign(
			ctx, session.SessionID, digest, true,
		)
		require.NoError(t, err)
	}

	return info
}

func signStaticSweeplessRound(t *testing.T, ctx context.Context,
	signer *testMuSig2Signer, locator keychain.KeyLocator,
	staticSwap *staticLoopInSwap,
	round *staticSweeplessRound) map[string]*swapserverrpc.
	ClientSweeplessSigningInfo {

	t.Helper()

	signers := [][]byte{
		staticSwap.address.clientKey.SerializeCompressed(),
		staticSwap.address.serverKey.pubKey.SerializeCompressed(),
	}
	prevFetcher := txscript.NewMultiPrevOutFetcher(staticSwap.prevOuts)
	sigHashes := txscript.NewTxSigHashes(round.tx, prevFetcher)
	infos := make(
		map[string]*swapserverrpc.ClientSweeplessSigningInfo,
		len(staticSwap.deposits),
	)
	for i, outpoint := range staticSwap.deposits {
		session, err := signer.MuSig2CreateSession(
			ctx, input.MuSig2Version100RC2, &locator, signers,
			lndclient.MuSig2TaprootTweakOpt(
				staticSwap.address.contract.RootHash[:], false,
			),
		)
		require.NoError(t, err)
		haveAllNonces, err := signer.MuSig2RegisterNonces(
			ctx, session.SessionID,
			[][musig2.PubNonceSize]byte{
				round.sessions[outpoint.String()].PublicNonce,
			},
		)
		require.NoError(t, err)
		require.True(t, haveAllNonces)

		digestBytes, err := txscript.CalcTaprootSignatureHash(
			sigHashes, txscript.SigHashDefault, round.tx, i,
			prevFetcher,
		)
		require.NoError(t, err)
		var digest [32]byte
		copy(digest[:], digestBytes)
		partialSig, err := signer.MuSig2Sign(
			ctx, session.SessionID, digest, true,
		)
		require.NoError(t, err)
		infos[outpoint.String()] = &swapserverrpc.
			ClientSweeplessSigningInfo{
			Nonce: append([]byte(nil), session.PublicNonce[:]...),
			Sig:   partialSig,
		}
	}

	return infos
}
