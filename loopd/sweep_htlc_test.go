package loopd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

// sweepHtlcTests is a collection of table tests for TestSweepHtlc.
var sweepHtlcTests = []struct {
	name           string
	amount         btcutil.Amount
	satPerVByte    uint32
	minRelayFee    chainfee.SatPerKWeight
	expectErrMsg   string
	expectLogs     []string
	expectRegister bool
	noSwap         bool
	publish        bool
	publishErr     bool
	signer         htlcSigner
	modifyReq      func(*looprpc.SweepHtlcRequest)
	mutateSwap     func(*loopdb.LoopOutContract)
	mutateTxOut    func(*wire.TxOut)
	sendConf       func(*test.ConfRegistration)
}{
	{
		name:           "success low fee",
		amount:         100_000,
		satPerVByte:    10,
		expectRegister: true,
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
			"sweephtlc: swap hash validated for %v",
			"sweephtlc: sweeping to %v with feerate %v sat/vbyte",
			"sweephtlc: signing sweep spending %v",
			"sweephtlc: witness assembled, tx size=%d vbytes",
		},
	},
	{
		name:           "success low fee, publish",
		amount:         100_000,
		satPerVByte:    10,
		expectRegister: true,
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
			"sweephtlc: swap hash validated for %v",
			"sweephtlc: sweeping to %v with feerate %v sat/vbyte",
			"sweephtlc: signing sweep spending %v",
			"sweephtlc: witness assembled, tx size=%d vbytes",
			"sweephtlc: published sweep %v",
		},
		publish: true,
	},
	{
		name:           "publish failure reported",
		amount:         100_000,
		satPerVByte:    10,
		expectRegister: true,
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
			"sweephtlc: swap hash validated for %v",
			"sweephtlc: sweeping to %v with feerate %v sat/vbyte",
			"sweephtlc: signing sweep spending %v",
			"sweephtlc: witness assembled, tx size=%d vbytes",
			"sweephtlc: publish failed for %v: %v",
		},
		publish:    true,
		publishErr: true,
	},
	{
		name:           "fee clamped over ratio",
		amount:         100_000,
		satPerVByte:    200,
		expectErrMsg:   "fee exceeds",
		expectRegister: true,
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
			"sweephtlc: swap hash validated for %v",
			"sweephtlc: sweeping to %v with feerate %v sat/vbyte",
		},
	},
	{
		name:   "clamped below min relay",
		amount: 10_000,
		// Will clamp further.
		satPerVByte:    5,
		minRelayFee:    chainfee.SatPerKWeight(1_000_000),
		expectErrMsg:   "fee too low for relay after clamp",
		expectRegister: true,
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
			"sweephtlc: swap hash validated for %v",
			"sweephtlc: sweeping to %v with feerate %v sat/vbyte",
		},
	},
	{
		name:           "missing outpoint",
		amount:         100_000,
		satPerVByte:    10,
		expectErrMsg:   "outpoint required",
		expectLogs:     []string{},
		expectRegister: false,
		modifyReq: func(req *looprpc.SweepHtlcRequest) {
			req.Outpoint = ""
		},
	},
	{
		name:           "missing htlc address",
		amount:         100_000,
		satPerVByte:    10,
		expectErrMsg:   "htlc_address required",
		expectLogs:     []string{},
		expectRegister: false,
		modifyReq: func(req *looprpc.SweepHtlcRequest) {
			req.HtlcAddress = ""
		},
	},
	{
		name:           "missing feerate",
		amount:         100_000,
		satPerVByte:    0,
		expectErrMsg:   "sat_per_vbyte required",
		expectLogs:     []string{},
		expectRegister: false,
	},
	{
		name:           "invalid htlc address",
		amount:         100_000,
		satPerVByte:    10,
		expectErrMsg:   "invalid htlc_address",
		expectLogs:     []string{},
		expectRegister: false,
		modifyReq: func(req *looprpc.SweepHtlcRequest) {
			req.HtlcAddress = "notanaddress"
		},
	},
	{
		name:           "no matching swap",
		amount:         100_000,
		satPerVByte:    10,
		expectErrMsg:   "no matching swap",
		expectRegister: false,
		noSwap:         true,
		expectLogs:     []string{},
	},
	{
		name:           "invalid initiation height",
		amount:         100_000,
		satPerVByte:    10,
		expectErrMsg:   "invalid initiation height",
		expectRegister: false,
		mutateSwap: func(contract *loopdb.LoopOutContract) {
			contract.InitiationHeight = 0
		},
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
		},
	},
	{
		name:           "conf ntfn error",
		amount:         100_000,
		satPerVByte:    10,
		expectErrMsg:   "conf ntfn",
		expectRegister: true,
		sendConf: func(reg *test.ConfRegistration) {
			reg.ErrChan <- errors.New("boom")
		},
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: conf ntfn error for %v: %v",
		},
	},
	{
		name:           "empty confirmation",
		amount:         100_000,
		satPerVByte:    10,
		expectErrMsg:   "confirmation notification was empty",
		expectRegister: true,
		sendConf: func(reg *test.ConfRegistration) {
			close(reg.ConfChan)
		},
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
		},
	},
	{
		name:           "confirmed transaction mismatch",
		amount:         100_000,
		satPerVByte:    10,
		expectErrMsg:   "confirmed transaction",
		expectRegister: true,
		sendConf: func(reg *test.ConfRegistration) {
			reg.ConfChan <- &chainntnfs.TxConfirmation{
				Tx: wire.NewMsgTx(2),
			}
		},
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
		},
	},
	{
		name:           "outpoint script mismatch",
		amount:         100_000,
		satPerVByte:    10,
		expectErrMsg:   "outpoint script does not match HTLC address",
		expectRegister: true,
		mutateTxOut: func(txOut *wire.TxOut) {
			txOut.PkScript = []byte{0x6a}
		},
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
		},
	},
	{
		name:           "fee exceeds htlc value",
		amount:         100_000,
		satPerVByte:    2_000_000,
		expectErrMsg:   "fee exceeds HTLC value",
		expectRegister: true,
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
			"sweephtlc: swap hash validated for %v",
			"sweephtlc: sweeping to %v with feerate %v sat/vbyte",
		},
	},
	{
		name:           "preimage mismatch",
		amount:         100_000,
		satPerVByte:    10,
		expectErrMsg:   "preimage does not match HTLC hash",
		expectRegister: true,
		modifyReq: func(req *looprpc.SweepHtlcRequest) {
			req.Preimage = bytes.Repeat([]byte{9}, 32)
		},
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
			"sweephtlc: swap hash validated for %v",
		},
	},
	{
		name:           "fallback to generated destination",
		amount:         100_000,
		satPerVByte:    10,
		expectRegister: true,
		mutateSwap: func(contract *loopdb.LoopOutContract) {
			contract.DestAddr = nil
		},
		expectLogs: []string{
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
			"sweephtlc: swap hash validated for %v",
			"sweephtlc: sweeping to %v with feerate %v sat/vbyte",
			"sweephtlc: signing sweep spending %v",
			"sweephtlc: witness assembled, tx size=%d vbytes",
		},
	},
	{
		name:           "invalid signer response",
		amount:         100_000,
		satPerVByte:    10,
		expectErrMsg:   "signer returned an invalid signature count",
		expectRegister: true,
		signer:         &emptySweepSigner{},
		expectLogs: []string{
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: using swap hash %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
			"sweephtlc: swap hash validated for %v",
			"sweephtlc: sweeping to %v with feerate %v sat/vbyte",
			"sweephtlc: signing sweep spending %v",
		},
	},
}

// TestSweepHtlcStatelessValidation covers the stateless request fields before
// any swap database or chain access is attempted.
func TestSweepHtlcStatelessValidation(t *testing.T) {
	defer test.Guard(t)()
	setLogger(btclog.Disabled)

	lnd := test.NewMockLnd()
	serverPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	_, clientKey := test.CreateKey(3)
	serverPubKey := serverPrivKey.PubKey().SerializeCompressed()
	clientPubKey := clientKey.SerializeCompressed()
	preimage := lntypes.Preimage{1, 2, 3, 4}
	htlc := statelessTestHtlc(
		t, lnd, preimage, 500, serverPubKey, clientPubKey,
	)

	newRequest := func() *looprpc.SweepHtlcRequest {
		return &looprpc.SweepHtlcRequest{
			Outpoint:    wire.OutPoint{}.String(),
			HtlcAddress: htlc.Address.EncodeAddress(),
			SatPerVbyte: 10,
			Preimage:    preimage[:],
			StatelessRecovery: &looprpc.StatelessRecovery{
				ServerPubkey:         serverPubKey,
				ClientPubkey:         clientPubKey,
				CltvExpiry:           500,
				SwapInitiationHeight: 123,
			},
		}
	}

	testCases := []struct {
		name     string
		modify   func(*looprpc.SweepHtlcRequest)
		expected string
	}{
		{
			name: "missing server key",
			modify: func(req *looprpc.SweepHtlcRequest) {
				req.StatelessRecovery.ServerPubkey = nil
			},
			expected: "both server_pubkey and client_pubkey " +
				"are required",
		},
		{
			name: "missing client key",
			modify: func(req *looprpc.SweepHtlcRequest) {
				req.StatelessRecovery.ClientPubkey = nil
			},
			expected: "both server_pubkey and client_pubkey " +
				"are required",
		},
		{
			name: "missing cltv expiry",
			modify: func(req *looprpc.SweepHtlcRequest) {
				req.StatelessRecovery.CltvExpiry = 0
			},
			expected: "cltv_expiry required in stateless mode",
		},
		{
			name: "missing initiation height",
			modify: func(req *looprpc.SweepHtlcRequest) {
				req.StatelessRecovery.SwapInitiationHeight = 0
			},
			expected: "swap_initiation_height required in " +
				"stateless mode",
		},
		{
			name: "missing preimage",
			modify: func(req *looprpc.SweepHtlcRequest) {
				req.Preimage = nil
			},
			expected: "preimage required in stateless mode",
		},
		{
			name: "server key length",
			modify: func(req *looprpc.SweepHtlcRequest) {
				req.StatelessRecovery.ServerPubkey = []byte{2}
			},
			expected: "server_pubkey must be 33 bytes",
		},
		{
			name: "server key encoding",
			modify: func(req *looprpc.SweepHtlcRequest) {
				recovery := req.StatelessRecovery
				recovery.ServerPubkey = bytes.Repeat(
					[]byte{0xff}, 33,
				)
			},
			expected: "invalid server_pubkey",
		},
		{
			name: "client key length",
			modify: func(req *looprpc.SweepHtlcRequest) {
				req.StatelessRecovery.ClientPubkey = []byte{2}
			},
			expected: "client_pubkey must be 33 bytes",
		},
		{
			name: "client key encoding",
			modify: func(req *looprpc.SweepHtlcRequest) {
				recovery := req.StatelessRecovery
				recovery.ClientPubkey = bytes.Repeat(
					[]byte{0xff}, 33,
				)
			},
			expected: "invalid client_pubkey",
		},
		{
			name: "cooperative payment address missing",
			modify: func(req *looprpc.SweepHtlcRequest) {
				req.Cooperative = &looprpc.CooperativeSweep{}
			},
			expected: "32-byte payment_address required",
		},
		{
			name: "cooperative payment address length",
			modify: func(req *looprpc.SweepHtlcRequest) {
				req.Cooperative = &looprpc.CooperativeSweep{
					PaymentAddress: make([]byte, 31),
				}
			},
			expected: "32-byte payment_address required",
		},
		{
			name: "cooperative signing unavailable",
			modify: func(req *looprpc.SweepHtlcRequest) {
				req.Cooperative = &looprpc.CooperativeSweep{
					PaymentAddress: make([]byte, 32),
				}
			},
			expected: "cooperative signing is unavailable",
		},
	}

	store := &rejectingLoopOutStore{}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			req := newRequest()
			testCase.modify(req)

			_, err := sweepHtlc(
				t.Context(), req, lnd.ChainParams, store,
				lnd.ChainNotifier, lnd.WalletKit,
				&realSweepSigner{}, nil, nil,
			)
			require.ErrorContains(t, err, testCase.expected)
		})
	}

	require.Zero(t, store.calls)

	statefulRequest := newRequest()
	statefulRequest.StatelessRecovery = nil
	statefulRequest.Cooperative = &looprpc.CooperativeSweep{
		PaymentAddress: make([]byte, 32),
	}
	_, err = sweepHtlc(
		t.Context(), statefulRequest, lnd.ChainParams, store,
		lnd.ChainNotifier, lnd.WalletKit, &realSweepSigner{}, nil,
		nil,
	)
	require.ErrorContains(
		t, err, "payment_address is only used in stateless mode",
	)
	require.Zero(t, store.calls)
	require.NoError(t, lnd.IsDone())
}

// TestSweepHtlcCooperative exercises both database-backed and stateless
// cooperative sweeps with two real MuSig2 signers.
func TestSweepHtlcCooperative(t *testing.T) {
	defer test.Guard(t)()
	setLogger(btclog.Disabled)

	const clientKeyIndex = int32(7)

	testCases := []struct {
		name      string
		stateless bool
	}{
		{
			name: "stateful",
		},
		{
			name:      "stateless",
			stateless: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			lnd := test.NewMockLnd()
			preimage := lntypes.Preimage{21, 22, 23, 24}
			swapHash := preimage.Hash()
			paymentAddr := [32]byte{31, 32, 33, 34}

			serverPrivKey, serverPubKey := test.CreateKey(8)
			clientPrivKey, clientPubKey := test.CreateKey(
				clientKeyIndex,
			)
			serverLocator := keychain.KeyLocator{
				Family: keychain.KeyFamily(swap.KeyFamily),
				Index:  8,
			}
			clientLocator := keychain.KeyLocator{
				Family: keychain.KeyFamily(swap.KeyFamily),
				Index:  uint32(clientKeyIndex),
			}

			var serverKey, clientKey [33]byte
			copy(serverKey[:], serverPubKey.SerializeCompressed())
			copy(clientKey[:], clientPubKey.SerializeCompressed())
			htlcKeys := loopdb.HtlcKeys{
				SenderScriptKey:        serverKey,
				SenderInternalPubKey:   serverKey,
				ReceiverScriptKey:      clientKey,
				ReceiverInternalPubKey: clientKey,
				ClientScriptKeyLocator: clientLocator,
			}
			contract := loopdb.SwapContract{
				Preimage:         preimage,
				AmountRequested:  100_000,
				HtlcKeys:         htlcKeys,
				CltvExpiry:       500,
				InitiationHeight: 123,
				ProtocolVersion:  loopdb.ProtocolVersionMuSig2,
			}
			htlc, err := utils.GetHtlc(
				swapHash, &contract, lnd.ChainParams,
			)
			require.NoError(t, err)

			fundingTx := wire.NewMsgTx(2)
			fundingTx.AddTxOut(&wire.TxOut{
				Value:    100_000,
				PkScript: htlc.PkScript,
			})
			outpoint := wire.OutPoint{Hash: fundingTx.TxHash()}
			destAddr, err := btcutil.NewAddressWitnessPubKeyHash(
				bytes.Repeat([]byte{5}, 20), lnd.ChainParams,
			)
			require.NoError(t, err)

			clientSigner := newRealMuSig2TestSigner(
				t, clientPrivKey, clientLocator,
			)
			serverSigner := newRealMuSig2TestSigner(
				t, serverPrivKey, serverLocator,
			)
			cooperativeSigner := newRealCooperativeServerSigner(
				t, serverSigner, serverLocator,
				loopdb.ProtocolVersionMuSig2, swapHash,
				paymentAddr, htlcKeys, htlc,
			)

			request := &looprpc.SweepHtlcRequest{
				Outpoint:    outpoint.String(),
				HtlcAddress: htlc.Address.EncodeAddress(),
				DestAddress: destAddr.EncodeAddress(),
				SatPerVbyte: 10,
				Cooperative: &looprpc.CooperativeSweep{},
			}
			var (
				store  loopOutStore
				wallet htlcWallet = lnd.WalletKit
			)
			if testCase.stateless {
				rejectingStore := &rejectingLoopOutStore{}
				countingWallet := &countingSweepWallet{
					htlcWallet: lnd.WalletKit,
				}
				store = rejectingStore
				wallet = countingWallet
				request.Preimage = preimage[:]
				request.StatelessRecovery = &looprpc.StatelessRecovery{
					ServerPubkey: serverKey[:],
					ClientPubkey: clientKey[:],
					CltvExpiry:   contract.CltvExpiry,
					SwapInitiationHeight: contract.
						InitiationHeight,
					KeyScanLimit: uint32(clientKeyIndex + 1),
				}
				request.Cooperative.PaymentAddress = paymentAddr[:]
				defer func() {
					require.Zero(t, rejectingStore.calls)
					require.Equal(
						t, int(clientKeyIndex+1),
						countingWallet.deriveCalls,
					)
				}()
			} else {
				invoice, err := zpay32.NewInvoice(
					lnd.ChainParams, swapHash, time.Now(),
					zpay32.Description("cooperative sweep"),
					zpay32.Amount(lnwire.NewMSatFromSatoshis(
						100_000,
					)),
					zpay32.PaymentAddr(paymentAddr),
				)
				require.NoError(t, err)
				payReq, err := test.EncodePayReq(invoice)
				require.NoError(t, err)

				storeMock := loopdb.NewStoreMock(t)
				storeMock.LoopOutSwaps[swapHash] =
					&loopdb.LoopOutContract{
						SwapContract: contract,
						DestAddr:     destAddr,
						SwapInvoice:  payReq,
					}
				store = storeMock
			}

			ctx, cancel := context.WithTimeout(
				t.Context(), 5*time.Second,
			)
			defer cancel()
			go sendSweepConfirmation(ctx, lnd, fundingTx)

			response, err := sweepHtlc(
				ctx, request, lnd.ChainParams, store,
				lnd.ChainNotifier, wallet, nil,
				clientSigner, cooperativeSigner.sign,
			)
			require.NoError(t, err)
			require.NotNil(t, response.GetNotRequested())
			require.Equal(t, 1, cooperativeSigner.calls)

			var sweepTx wire.MsgTx
			err = sweepTx.Deserialize(bytes.NewReader(response.SweepTx))
			require.NoError(t, err)
			require.Len(t, sweepTx.TxIn, 1)
			require.Len(t, sweepTx.TxIn[0].Witness, 1)
			require.Len(t, sweepTx.TxIn[0].Witness[0], 64)
			require.NoError(t, verifySweepHtlcWitness(
				&sweepTx, fundingTx.TxOut[0],
			))
			require.Equal(
				t, int64(100_000-response.FeeSats),
				sweepTx.TxOut[0].Value,
			)

			select {
			case <-lnd.TxPublishChannel:
				t.Fatal("unexpected publish")

			case <-time.After(100 * time.Millisecond):
			}
			require.NoError(t, lnd.IsDone())
		})
	}
}

// TestSweepHtlcStateless exercises public-key signing and the bounded key scan
// fallback with real Schnorr signatures and script execution.
func TestSweepHtlcStateless(t *testing.T) {
	defer test.Guard(t)()
	setLogger(btclog.Disabled)

	const clientKeyIndex = int32(7)

	testCases := []struct {
		name             string
		directMode       string
		walletErr        error
		keyScanLimit     uint32
		expectErr        string
		expectKeyScans   int
		expectLocatorSig int
	}{
		{
			name:         "lnd knows public key",
			keyScanLimit: 1,
		},
		{
			name:             "signer error scan succeeds",
			directMode:       "signer error",
			keyScanLimit:     uint32(clientKeyIndex + 1),
			expectKeyScans:   int(clientKeyIndex + 1),
			expectLocatorSig: 1,
		},
		{
			name:             "zero scan limit uses default",
			directMode:       "signer error",
			expectKeyScans:   int(clientKeyIndex + 1),
			expectLocatorSig: 1,
		},
		{
			name:             "legacy wrong key scan succeeds",
			directMode:       "wrong signature",
			keyScanLimit:     uint32(clientKeyIndex + 1),
			expectKeyScans:   int(clientKeyIndex + 1),
			expectLocatorSig: 1,
		},
		{
			name:           "key outside scan range",
			directMode:     "signer error",
			keyScanLimit:   uint32(clientKeyIndex),
			expectErr:      "searched key family 99 indices 0-6",
			expectKeyScans: int(clientKeyIndex),
		},
		{
			name:         "signer error reports scan failure",
			directMode:   "signer error",
			walletErr:    errors.New("wallet locked"),
			keyScanLimit: uint32(clientKeyIndex + 1),
			expectErr: "derive client key at family 99 index 0: " +
				"wallet locked",
			expectKeyScans: 1,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			logger := newFormatLogger()
			setLogger(logger)

			lnd := test.NewMockLnd()
			store := &rejectingLoopOutStore{}
			wallet := &countingSweepWallet{
				htlcWallet: lnd.WalletKit,
				deriveErr:  testCase.walletErr,
			}

			serverPrivKey, err := btcec.NewPrivateKey()
			require.NoError(t, err)
			clientPrivKey, clientKey := test.CreateKey(
				clientKeyIndex,
			)
			wrongPrivKey, _ := test.CreateKey(100)
			serverPubKey := serverPrivKey.PubKey().
				SerializeCompressed()
			clientPubKey := clientKey.SerializeCompressed()
			preimage := lntypes.Preimage{1, 2, 3, 4}

			htlc := statelessTestHtlc(
				t, lnd, preimage, 500, serverPubKey,
				clientPubKey,
			)
			fundingTx := wire.NewMsgTx(2)
			fundingTx.AddTxOut(&wire.TxOut{
				Value:    100_000,
				PkScript: htlc.PkScript,
			})
			outpoint := wire.OutPoint{Hash: fundingTx.TxHash()}

			destAddr, err := btcutil.NewAddressWitnessPubKeyHash(
				bytes.Repeat([]byte{2}, 20), lnd.ChainParams,
			)
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(
				t.Context(), 5*time.Second,
			)
			defer cancel()
			go sendSweepConfirmation(ctx, lnd, fundingTx)

			signer := &realSweepSigner{
				privateKey:        clientPrivKey,
				locatorPrivateKey: clientPrivKey,
				expectedPubKey:    clientPubKey,
				expectedKeyLocator: &keychain.KeyLocator{
					Family: keychain.KeyFamily(
						swap.KeyFamily,
					),
					Index: uint32(clientKeyIndex),
				},
			}
			switch testCase.directMode {
			case "signer error":
				signer.directErr = errors.New("signer failed")

			case "wrong signature":
				signer.privateKey = wrongPrivKey
			}

			recovery := &looprpc.StatelessRecovery{
				ServerPubkey:         serverPubKey,
				ClientPubkey:         clientPubKey,
				CltvExpiry:           500,
				SwapInitiationHeight: 123,
				KeyScanLimit:         testCase.keyScanLimit,
			}
			request := &looprpc.SweepHtlcRequest{
				Outpoint:          outpoint.String(),
				HtlcAddress:       htlc.Address.EncodeAddress(),
				DestAddress:       destAddr.EncodeAddress(),
				SatPerVbyte:       10,
				Preimage:          preimage[:],
				StatelessRecovery: recovery,
			}
			resp, err := sweepHtlc(
				ctx, request, lnd.ChainParams, store,
				lnd.ChainNotifier,
				wallet, signer, nil, nil,
			)
			if testCase.expectErr != "" {
				require.ErrorContains(
					t, err, testCase.expectErr,
				)
				require.Nil(t, resp)
			} else {
				require.NoError(t, err)
				expectedVerifyLog := "sweephtlc: verifying " +
					"stateless sweep witness"
				if testCase.expectLocatorSig > 0 {
					expectedVerifyLog += " after " +
						"key recovery"
				}
				require.Contains(
					t, logger.formats, expectedVerifyLog,
				)
				verifiedLog := "sweephtlc: stateless sweep " +
					"witness verified"
				require.Contains(t, logger.formats, verifiedLog)

				var sweepTx wire.MsgTx
				require.NoError(t, sweepTx.Deserialize(
					bytes.NewReader(resp.SweepTx),
				))
				require.NoError(t, verifySweepHtlcWitness(
					&sweepTx, fundingTx.TxOut[0],
				))
			}

			require.Zero(t, store.calls)
			require.Equal(t, 1, signer.directCalls)
			require.Equal(
				t, testCase.expectLocatorSig,
				signer.keyLocatorCalls,
			)
			require.Equal(
				t, testCase.expectKeyScans, wallet.deriveCalls,
			)
			require.NoError(t, lnd.IsDone())
		})
	}
}

// TestFindStatelessSweepKeyProgress verifies that a long scan reports
// progress before exhausting its search range.
func TestFindStatelessSweepKeyProgress(t *testing.T) {
	defer test.Guard(t)()

	logger := newFormatLogger()
	setLogger(logger)
	lnd := test.NewMockLnd()
	targetKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	keyScanLimit := uint32(statelessRecoveryKeyScanLogInterval)

	_, err = findStatelessSweepKey(
		t.Context(), lnd.WalletKit, targetKey.PubKey(), keyScanLimit,
	)
	require.ErrorContains(t, err, "searched key family 99 indices 0-1999")
	require.Equal(t, []string{
		"sweephtlc: scanned %d of %d family-%d keys",
	}, logger.formats)
	require.NoError(t, lnd.IsDone())
}

// TestSweepHtlcStatelessRejectsInvalidSignature ensures a signer cannot return
// a signature for a different key and still produce a recovery transaction.
func TestSweepHtlcStatelessRejectsInvalidSignature(t *testing.T) {
	defer test.Guard(t)()
	setLogger(btclog.Disabled)

	lnd := test.NewMockLnd()
	serverPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	_, clientKey := test.CreateKey(8)
	wrongPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPubKey := serverPrivKey.PubKey().SerializeCompressed()
	clientPubKey := clientKey.SerializeCompressed()
	preimage := lntypes.Preimage{5, 6, 7, 8}
	htlc := statelessTestHtlc(
		t, lnd, preimage, 500, serverPubKey, clientPubKey,
	)

	fundingTx := wire.NewMsgTx(2)
	fundingTx.AddTxOut(&wire.TxOut{
		Value:    100_000,
		PkScript: htlc.PkScript,
	})
	outpoint := wire.OutPoint{Hash: fundingTx.TxHash()}

	destAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		bytes.Repeat([]byte{3}, 20), lnd.ChainParams,
	)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go sendSweepConfirmation(ctx, lnd, fundingTx)

	_, err = sweepHtlc(
		ctx, &looprpc.SweepHtlcRequest{
			Outpoint:    outpoint.String(),
			HtlcAddress: htlc.Address.EncodeAddress(),
			DestAddress: destAddr.EncodeAddress(),
			SatPerVbyte: 10,
			Preimage:    preimage[:],
			StatelessRecovery: &looprpc.StatelessRecovery{
				ServerPubkey:         serverPubKey,
				ClientPubkey:         clientPubKey,
				CltvExpiry:           500,
				SwapInitiationHeight: 123,
			},
		}, lnd.ChainParams, &rejectingLoopOutStore{},
		lnd.ChainNotifier, lnd.WalletKit, &realSweepSigner{
			privateKey:     wrongPrivKey,
			expectedPubKey: clientPubKey,
		}, nil, nil,
	)
	require.ErrorContains(t, err, "invalid signature for client_pubkey")
	require.NoError(t, lnd.IsDone())
}

// TestSweepHtlcStatelessAddressMismatch verifies that key reconstruction is
// checked before any database or chain access and reports both addresses.
func TestSweepHtlcStatelessAddressMismatch(t *testing.T) {
	defer test.Guard(t)()
	setLogger(btclog.Disabled)

	lnd := test.NewMockLnd()
	serverPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	otherServerPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPubKey := serverPrivKey.PubKey().SerializeCompressed()
	clientPubKey := clientPrivKey.PubKey().SerializeCompressed()
	otherServerPubKey := otherServerPrivKey.PubKey().SerializeCompressed()
	preimage := lntypes.Preimage{9, 10, 11, 12}

	providedHtlc := statelessTestHtlc(
		t, lnd, preimage, 500, serverPubKey, clientPubKey,
	)
	generatedHtlc := statelessTestHtlc(
		t, lnd, preimage, 500, otherServerPubKey, clientPubKey,
	)
	store := &rejectingLoopOutStore{}

	_, err = sweepHtlc(
		t.Context(), &looprpc.SweepHtlcRequest{
			Outpoint:    wire.OutPoint{}.String(),
			HtlcAddress: providedHtlc.Address.EncodeAddress(),
			SatPerVbyte: 10,
			Preimage:    preimage[:],
			StatelessRecovery: &looprpc.StatelessRecovery{
				ServerPubkey:         otherServerPubKey,
				ClientPubkey:         clientPubKey,
				CltvExpiry:           500,
				SwapInitiationHeight: 123,
			},
		}, lnd.ChainParams, store, lnd.ChainNotifier,
		lnd.WalletKit, &realSweepSigner{}, nil, nil,
	)
	require.ErrorContains(
		t, err, providedHtlc.Address.EncodeAddress(),
	)
	require.ErrorContains(
		t, err, generatedHtlc.Address.EncodeAddress(),
	)
	require.Zero(t, store.calls)
	require.NoError(t, lnd.IsDone())
}

// TestSweepHtlcStatelessOnChainAddressMismatch verifies that the actual
// outpoint script is compared with the reconstructed HTLC and that both
// addresses are included in the error.
func TestSweepHtlcStatelessOnChainAddressMismatch(t *testing.T) {
	defer test.Guard(t)()
	setLogger(btclog.Disabled)

	lnd := test.NewMockLnd()
	serverPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPubKey := serverPrivKey.PubKey().SerializeCompressed()
	clientPubKey := clientPrivKey.PubKey().SerializeCompressed()
	preimage := lntypes.Preimage{13, 14, 15, 16}
	htlc := statelessTestHtlc(
		t, lnd, preimage, 500, serverPubKey, clientPubKey,
	)
	observedAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		bytes.Repeat([]byte{4}, 20), lnd.ChainParams,
	)
	require.NoError(t, err)
	observedScript, err := txscript.PayToAddrScript(observedAddr)
	require.NoError(t, err)

	fundingTx := wire.NewMsgTx(2)
	fundingTx.AddTxOut(&wire.TxOut{
		Value:    100_000,
		PkScript: observedScript,
	})
	outpoint := wire.OutPoint{Hash: fundingTx.TxHash()}
	store := &rejectingLoopOutStore{}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go sendSweepConfirmation(ctx, lnd, fundingTx)

	_, err = sweepHtlc(
		ctx, &looprpc.SweepHtlcRequest{
			Outpoint:    outpoint.String(),
			HtlcAddress: htlc.Address.EncodeAddress(),
			SatPerVbyte: 10,
			Preimage:    preimage[:],
			StatelessRecovery: &looprpc.StatelessRecovery{
				ServerPubkey:         serverPubKey,
				ClientPubkey:         clientPubKey,
				CltvExpiry:           500,
				SwapInitiationHeight: 123,
			},
		}, lnd.ChainParams, store, lnd.ChainNotifier,
		lnd.WalletKit, &realSweepSigner{}, nil, nil,
	)
	require.ErrorContains(t, err, observedAddr.EncodeAddress())
	require.ErrorContains(t, err, htlc.Address.EncodeAddress())
	require.Zero(t, store.calls)
	require.NoError(t, lnd.IsDone())
}

// statelessTestHtlc constructs the latest Loop Out HTLC from the public data
// accepted by stateless sweep mode.
func statelessTestHtlc(t *testing.T, lnd *test.LndMockServices,
	preimage lntypes.Preimage, cltvExpiry int32, serverPubKey,
	clientPubKey []byte) *swap.Htlc {

	var serverKey, clientKey [33]byte
	copy(serverKey[:], serverPubKey)
	copy(clientKey[:], clientPubKey)

	htlc, err := utils.GetHtlc(
		preimage.Hash(), &loopdb.SwapContract{
			CltvExpiry:      cltvExpiry,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys: loopdb.HtlcKeys{
				SenderScriptKey:        serverKey,
				SenderInternalPubKey:   serverKey,
				ReceiverScriptKey:      clientKey,
				ReceiverInternalPubKey: clientKey,
			},
		}, lnd.ChainParams,
	)
	require.NoError(t, err)

	return htlc
}

// sendSweepConfirmation responds to a confirmation registration with the
// supplied funding transaction.
func sendSweepConfirmation(ctx context.Context, lnd *test.LndMockServices,
	fundingTx *wire.MsgTx) {

	select {
	case registration := <-lnd.RegisterConfChannel:
		registration.ConfChan <- &chainntnfs.TxConfirmation{
			Tx: fundingTx,
		}

	case <-ctx.Done():
	}
}

// rejectingLoopOutStore proves that stateless recovery does not query the
// Loop database.
type rejectingLoopOutStore struct {
	calls int
}

// countingSweepWallet records bounded key scan derivations.
type countingSweepWallet struct {
	htlcWallet

	deriveCalls int
	deriveErr   error
}

// DeriveKey records and forwards a key derivation.
func (w *countingSweepWallet) DeriveKey(ctx context.Context,
	locator *keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	w.deriveCalls++
	if w.deriveErr != nil {
		return nil, w.deriveErr
	}

	return w.htlcWallet.DeriveKey(ctx, locator)
}

// FetchLoopOutSwaps always fails if the stateless path accesses the store.
func (s *rejectingLoopOutStore) FetchLoopOutSwaps(context.Context) (
	[]*loopdb.LoopOut, error) {

	s.calls++

	return nil, errors.New("unexpected swap database access")
}

// realSweepSigner produces real Schnorr signatures for the Taproot script
// spend used by stateless recovery.
type realSweepSigner struct {
	privateKey         *btcec.PrivateKey
	locatorPrivateKey  *btcec.PrivateKey
	directErr          error
	expectedPubKey     []byte
	expectedKeyLocator *keychain.KeyLocator
	directCalls        int
	keyLocatorCalls    int
}

// realMuSig2TestSigner adapts lnd's in-memory MuSig2 session manager to the
// lndclient signer interface. The manager performs all nonce and signature
// operations with the configured real private key.
type realMuSig2TestSigner struct {
	manager *input.MusigSessionManager
}

// newRealMuSig2TestSigner creates a real MuSig2 signer for one key locator.
func newRealMuSig2TestSigner(t *testing.T, privateKey *btcec.PrivateKey,
	locator keychain.KeyLocator) *realMuSig2TestSigner {

	t.Helper()

	keyFetcher := func(keyDesc *keychain.KeyDescriptor) (
		*btcec.PrivateKey, error) {

		if keyDesc.KeyLocator != locator {
			return nil, fmt.Errorf("unexpected key locator: %v",
				keyDesc.KeyLocator)
		}

		return privateKey, nil
	}

	return &realMuSig2TestSigner{
		manager: input.NewMusigSessionManager(keyFetcher),
	}
}

// MuSig2CreateSession creates a real signing session.
func (s *realMuSig2TestSigner) MuSig2CreateSession(_ context.Context,
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

// MuSig2RegisterNonces registers real public nonces with a session.
func (s *realMuSig2TestSigner) MuSig2RegisterNonces(_ context.Context,
	sessionID [32]byte, nonces [][musig2.PubNonceSize]byte) (
	bool, error) {

	return s.manager.MuSig2RegisterNonces(
		input.MuSig2SessionID(sessionID), nonces,
	)
}

// MuSig2Sign creates a real partial MuSig2 signature.
func (s *realMuSig2TestSigner) MuSig2Sign(_ context.Context,
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

// MuSig2CombineSig combines real partial signatures into a Schnorr signature.
func (s *realMuSig2TestSigner) MuSig2CombineSig(_ context.Context,
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

// MuSig2Cleanup removes a real signing session.
func (s *realMuSig2TestSigner) MuSig2Cleanup(_ context.Context,
	sessionID [32]byte) error {

	return s.manager.MuSig2Cleanup(input.MuSig2SessionID(sessionID))
}

// realCooperativeServerSigner emulates the existing server signing RPC with a
// second real MuSig2 signer.
type realCooperativeServerSigner struct {
	t               *testing.T
	signer          *realMuSig2TestSigner
	locator         keychain.KeyLocator
	protocolVersion loopdb.ProtocolVersion
	swapHash        lntypes.Hash
	paymentAddr     [32]byte
	htlcKeys        loopdb.HtlcKeys
	htlc            *swap.Htlc
	calls           int
}

// newRealCooperativeServerSigner creates a real cooperative server signer.
func newRealCooperativeServerSigner(t *testing.T,
	signer *realMuSig2TestSigner, locator keychain.KeyLocator,
	protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
	paymentAddr [32]byte, htlcKeys loopdb.HtlcKeys,
	htlc *swap.Htlc) *realCooperativeServerSigner {

	t.Helper()

	return &realCooperativeServerSigner{
		t:               t,
		signer:          signer,
		locator:         locator,
		protocolVersion: protocolVersion,
		swapHash:        swapHash,
		paymentAddr:     paymentAddr,
		htlcKeys:        htlcKeys,
		htlc:            htlc,
	}
}

// sign validates the server request and creates a real server partial
// signature over the PSBT transaction.
func (s *realCooperativeServerSigner) sign(ctx context.Context,
	protocolVersion loopdb.ProtocolVersion, swapHash lntypes.Hash,
	paymentAddr [32]byte, clientNonce, sweepTxPsbt []byte) (
	[]byte, []byte, error) {

	s.calls++
	require.Equal(s.t, s.protocolVersion, protocolVersion)
	require.Equal(s.t, s.swapHash, swapHash)
	require.Equal(s.t, s.paymentAddr, paymentAddr)
	require.Len(s.t, clientNonce, musig2.PubNonceSize)

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(sweepTxPsbt), false,
	)
	require.NoError(s.t, err)
	require.Len(s.t, packet.Inputs, 1)
	require.NotNil(s.t, packet.Inputs[0].WitnessUtxo)

	prevOut := packet.Inputs[0].WitnessUtxo
	prevOutFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(
		packet.UnsignedTx, prevOutFetcher,
	)
	sigHash, err := txscript.CalcTaprootSignatureHash(
		sigHashes, txscript.SigHashDefault, packet.UnsignedTx, 0,
		prevOutFetcher,
	)
	require.NoError(s.t, err)

	htlcV3, ok := s.htlc.HtlcScript.(*swap.HtlcScriptV3)
	require.True(s.t, ok)
	var nonce [musig2.PubNonceSize]byte
	copy(nonce[:], clientNonce)

	muSig2Version := input.MuSig2Version100RC2
	signers := [][]byte{
		s.htlcKeys.SenderInternalPubKey[:],
		s.htlcKeys.ReceiverInternalPubKey[:],
	}
	if protocolVersion < loopdb.ProtocolVersionMuSig2 {
		muSig2Version = input.MuSig2Version040
		signers = [][]byte{
			s.htlcKeys.SenderInternalPubKey[1:],
			s.htlcKeys.ReceiverInternalPubKey[1:],
		}
	}

	session, err := s.signer.MuSig2CreateSession(
		ctx, muSig2Version, &s.locator, signers,
		lndclient.MuSig2TaprootTweakOpt(
			htlcV3.RootHash[:], false,
		),
		lndclient.MuSig2NonceOpt(
			[][musig2.PubNonceSize]byte{nonce},
		),
	)
	require.NoError(s.t, err)
	require.True(s.t, session.HaveAllNonces)

	var message [32]byte
	copy(message[:], sigHash)
	partialSig, err := s.signer.MuSig2Sign(
		ctx, session.SessionID, message, true,
	)
	require.NoError(s.t, err)

	return session.PublicNonce[:], partialSig, nil
}

// emptySweepSigner returns an invalid empty signature response.
type emptySweepSigner struct{}

// SignOutputRaw returns no signatures.
func (*emptySweepSigner) SignOutputRaw(context.Context, *wire.MsgTx,
	[]*lndclient.SignDescriptor, []*wire.TxOut) ([][]byte, error) {

	return nil, nil
}

// SignOutputRawKeyLocator returns no signatures.
func (*emptySweepSigner) SignOutputRawKeyLocator(context.Context, *wire.MsgTx,
	[]*lndclient.SignDescriptor, []*wire.TxOut) ([][]byte, error) {

	return nil, nil
}

// SignOutputRaw signs each descriptor with the configured private key.
func (s *realSweepSigner) SignOutputRaw(_ context.Context, tx *wire.MsgTx,
	signDescriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([][]byte, error) {

	s.directCalls++
	if s.directErr != nil {
		return nil, s.directErr
	}

	return s.sign(tx, signDescriptors, prevOutputs, s.privateKey, false)
}

// SignOutputRawKeyLocator signs using the recovered key locator.
func (s *realSweepSigner) SignOutputRawKeyLocator(_ context.Context,
	tx *wire.MsgTx, signDescriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([][]byte, error) {

	s.keyLocatorCalls++

	privateKey := s.locatorPrivateKey
	if privateKey == nil {
		privateKey = s.privateKey
	}

	return s.sign(tx, signDescriptors, prevOutputs, privateKey, true)
}

// sign produces real Schnorr signatures with the selected private key.
func (s *realSweepSigner) sign(tx *wire.MsgTx,
	signDescriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut, privateKey *btcec.PrivateKey,
	requireLocator bool) ([][]byte, error) {

	if privateKey == nil {
		return nil, errors.New("private key unavailable")
	}
	if len(prevOutputs) != len(tx.TxIn) {
		return nil, errors.New("previous output count mismatch")
	}

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, txIn := range tx.TxIn {
		prevOutFetcher.AddPrevOut(
			txIn.PreviousOutPoint, prevOutputs[i],
		)
	}
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	signatures := make([][]byte, len(signDescriptors))
	for i, signDesc := range signDescriptors {
		if signDesc.SignMethod != input.TaprootScriptSpendSignMethod {
			return nil, fmt.Errorf("unexpected sign method: %v",
				signDesc.SignMethod)
		}
		if signDesc.KeyDesc.PubKey == nil {
			return nil, errors.New("signing public key missing")
		}
		serializedKey := signDesc.KeyDesc.PubKey.SerializeCompressed()
		if !bytes.Equal(serializedKey, s.expectedPubKey) {
			return nil, errors.New("unexpected signing public key")
		}
		if requireLocator && s.expectedKeyLocator != nil &&
			signDesc.KeyDesc.KeyLocator != *s.expectedKeyLocator {

			return nil, fmt.Errorf(
				"unexpected signing key locator: %v",
				signDesc.KeyDesc.KeyLocator,
			)
		}

		tapLeaf := txscript.NewBaseTapLeaf(
			signDesc.WitnessScript,
		)
		signature, err := txscript.RawTxInTapscriptSignature(
			tx, sigHashes, signDesc.InputIndex,
			signDesc.Output.Value, signDesc.Output.PkScript,
			tapLeaf, signDesc.HashType, privateKey,
		)
		if err != nil {
			return nil, err
		}

		signatures[i] = signature
	}

	return signatures, nil
}

// TestSweepHtlc runs a table of happy-path and fee-related rejection cases for
// the sweep helper.
func TestSweepHtlc(t *testing.T) {
	// shortDelay is used to check that nothing is produced from a channel.
	const shortDelay = 100 * time.Millisecond

	for _, tc := range sweepHtlcTests {
		t.Run(tc.name, func(t *testing.T) {
			// Catch leaked goroutines and constrain test time.
			defer test.Guard(t)()

			// Fresh logger per test to capture emitted formats.
			logger := newFormatLogger()
			setLogger(logger)

			// Base mocks for wallet/notifier/signer.
			lnd := test.NewMockLnd()
			if tc.publishErr {
				lnd.PublishHandler = func(ctx context.Context,
					_ *wire.MsgTx, _ string) error {

					return errors.New("publish-fail")
				}
			}
			if tc.minRelayFee != 0 {
				lnd.SetMinRelayFee(tc.minRelayFee)
			}
			store := loopdb.NewStoreMock(t)

			preimage := lntypes.Preimage{1, 2, 3, 4}
			swapHash := preimage.Hash()

			_, senderPub := test.CreateKey(0)
			_, receiverPub := test.CreateKey(1)

			var senderKey, receiverKey [33]byte
			copy(senderKey[:], senderPub.SerializeCompressed())
			copy(receiverKey[:], receiverPub.SerializeCompressed())

			htlcKeys := loopdb.HtlcKeys{
				SenderScriptKey:   senderKey,
				ReceiverScriptKey: receiverKey,
				ClientScriptKeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamily(
						swap.KeyFamily,
					),
					Index: 0,
				},
			}

			swapContract := loopdb.SwapContract{
				Preimage:         preimage,
				AmountRequested:  tc.amount,
				HtlcKeys:         htlcKeys,
				CltvExpiry:       500,
				InitiationHeight: 123,
				ProtocolVersion:  loopdb.ProtocolVersionHtlcV2,
			}

			destAddr, err := btcutil.NewAddressWitnessPubKeyHash(
				bytes.Repeat([]byte{1}, 20), lnd.ChainParams,
			)
			require.NoError(t, err)

			loopOut := &loopdb.LoopOut{
				Loop: loopdb.Loop{
					Hash: swapHash,
				},
				Contract: &loopdb.LoopOutContract{
					SwapContract: swapContract,
					DestAddr:     destAddr,
				},
			}

			// Store the swap unless this case disables it.
			if tc.mutateSwap != nil {
				tc.mutateSwap(loopOut.Contract)
			}
			if !tc.noSwap {
				store.LoopOutSwaps[swapHash] = loopOut.Contract
			}

			// Build HTLC details and funding tx.
			htlc, err := utils.GetHtlc(
				swapHash, &loopOut.Contract.SwapContract,
				lnd.ChainParams,
			)
			require.NoError(t, err)

			fundingTx := wire.NewMsgTx(2)
			txOut := &wire.TxOut{
				Value: int64(
					loopOut.Contract.AmountRequested,
				),
				PkScript: htlc.PkScript,
			}
			if tc.mutateTxOut != nil {
				tc.mutateTxOut(txOut)
			}
			fundingTx.AddTxOut(txOut)
			fundingHash := fundingTx.TxHash()
			outpoint := wire.OutPoint{Hash: fundingHash, Index: 0}

			ctx, cancel := context.WithTimeout(
				t.Context(), 5*time.Second,
			)
			defer cancel()

			signer := tc.signer
			if signer == nil {
				signer = lnd.Signer

				// Drain signer requests to avoid blocking.
				go func() {
					select {
					case <-lnd.SignOutputRawChannel:

					case <-ctx.Done():
					}
				}()
			}

			pubChan := make(chan *wire.MsgTx, 1)

			// If publish is requested, drain TxPublishChannel so
			// the mock PublishTransaction does not block.
			if tc.publish {
				go func() {
					select {
					case tx := <-lnd.TxPublishChannel:
						pubChan <- tx

					case <-ctx.Done():
					}
				}()
			}

			// Handle confirmation registration caused by the call.
			if tc.expectRegister {
				// Consume notifier registration.
				go func() {
					var reg *test.ConfRegistration
					select {
					case reg = <-lnd.RegisterConfChannel:
						// Got registration.

					case <-ctx.Done():
						return
					}

					// Either send an error or a
					// confirmation.
					if tc.sendConf != nil {
						tc.sendConf(reg)

						return
					}

					conf := &chainntnfs.TxConfirmation{
						Tx: fundingTx,
					}
					reg.ConfChan <- conf
				}()
			}

			// Build request with optional mutation.
			req := &looprpc.SweepHtlcRequest{
				Outpoint:    outpoint.String(),
				SatPerVbyte: tc.satPerVByte,
				Publish:     tc.publish,
				HtlcAddress: htlc.Address.String(),
				DestAddress: "",
				Preimage:    nil,
			}
			if tc.modifyReq != nil {
				tc.modifyReq(req)
			}

			// Invoke sweepHtlc and forward the result.
			resp, err := sweepHtlc(
				ctx, req, lnd.ChainParams, store,
				lnd.ChainNotifier, lnd.WalletKit,
				signer, nil, nil,
			)

			// Handle confirmation registration caused by the call
			// when not expected.
			if !tc.expectRegister {
				select {
				case reg := <-lnd.RegisterConfChannel:
					t.Fatalf("unexpected registration: %+v",
						reg)

				case <-time.After(shortDelay):
				}
			}

			// Make sure it produced the expected logs.
			logs := logger.formats
			if logs == nil {
				logs = []string{}
			}
			require.Equal(t, tc.expectLogs, logs)

			// Ensure all mock channels are drained.
			defer require.NoError(t, lnd.IsDone())

			// Error path.
			if tc.expectErrMsg != "" {
				require.ErrorContains(t, err, tc.expectErrMsg)

				return
			}

			// Success path.
			require.NoError(t, err)

			// Parse the produced signed transaction.
			require.NotEmpty(t, resp.SweepTx)
			var sweepTx wire.MsgTx
			err = sweepTx.Deserialize(bytes.NewReader(resp.SweepTx))
			require.NoError(t, err)
			require.Equal(
				t, outpoint, sweepTx.TxIn[0].PreviousOutPoint,
			)
			require.NotEmpty(t, sweepTx.TxIn[0].Witness)

			// Verify that the sweep uses the stored destination address and
			// only falls back to wallet address generation when the swap
			// record does not define one.
			expectedAddr := loopOut.Contract.DestAddr
			if req.DestAddress != "" {
				expectedAddr, err = btcutil.DecodeAddress(
					req.DestAddress, lnd.ChainParams,
				)
				require.NoError(t, err)
			}
			if expectedAddr == nil {
				expectedAddr, err = lnd.WalletKit.NextAddr(
					ctx, lnwallet.DefaultAccountName,
					walletrpc.AddressType_TAPROOT_PUBKEY, false,
				)
				require.NoError(t, err)
			}

			expectedPkScript, err := txscript.PayToAddrScript(
				expectedAddr,
			)
			require.NoError(t, err)
			require.Len(t, sweepTx.TxOut, 1)
			require.Equal(
				t, expectedPkScript, sweepTx.TxOut[0].PkScript,
			)

			if tc.publish {
				// For publish=true we should see a
				// publish (or a publish failure
				// response which skips broadcast).
				select {
				case tx := <-pubChan:
					require.NotNil(t, tx)

				case <-time.After(shortDelay):
					if !tc.publishErr {
						t.Fatal("expected publish")
					}
				}
			} else {
				// For publish=false we should not
				// publish.
				select {
				case <-lnd.TxPublishChannel:
					t.Fatal("unexpected publish")

				case <-time.After(shortDelay):
				}
			}
		})
	}
}

// formatLogger captures format strings passed to the logger interface so we
// can assert on log invocations.
type formatLogger struct {
	btclog.Logger

	formats []string
}

// newFormatLogger builds a logger that records format strings while discarding
// actual log output.
func newFormatLogger() *formatLogger {
	return &formatLogger{Logger: btclog.Disabled}
}

// record stores the raw format string.
func (f *formatLogger) record(format string) {
	f.formats = append(f.formats, format)
}

// Tracef logs a trace and records its format.
func (f *formatLogger) Tracef(format string, params ...any) {
	f.record(format)
	f.Logger.Tracef(format, params...)
}

// Debugf logs a debug message and records its format.
func (f *formatLogger) Debugf(format string, params ...any) {
	f.record(format)
	f.Logger.Debugf(format, params...)
}

// Infof logs an info message and records its format.
func (f *formatLogger) Infof(format string, params ...any) {
	f.record(format)
	f.Logger.Infof(format, params...)
}

// Warnf logs a warning and records its format.
func (f *formatLogger) Warnf(format string, params ...any) {
	f.record(format)
	f.Logger.Warnf(format, params...)
}

// Errorf logs an error and records its format.
func (f *formatLogger) Errorf(format string, params ...any) {
	f.record(format)
	f.Logger.Errorf(format, params...)
}

// Criticalf logs a critical message and records its format.
func (f *formatLogger) Criticalf(format string, params ...any) {
	f.record(format)
	f.Logger.Criticalf(format, params...)
}
