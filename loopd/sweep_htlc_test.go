package loopd

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: matched swap %v at height hint %v",
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
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: matched swap %v at height hint %v",
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
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: matched swap %v at height hint %v",
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
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: matched swap %v at height hint %v",
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
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: matched swap %v at height hint %v",
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
		expectLogs: []string{
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
		},
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
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: matched swap %v at height hint %v",
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
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: matched swap %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: conf ntfn error for %v: %v",
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
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: matched swap %v at height hint %v",
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
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: matched swap %v at height hint %v",
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
			"sweephtlc: generated new destination address: %v",
			"sweephtlc: start sweep for %v -> %v",
			"sweephtlc: matched swap %v at height hint %v",
			"sweephtlc: registering conf ntfn for %v hint=%v",
			"sweephtlc: waiting for confirmation of %v",
			"sweephtlc: funding confirmed at height %v",
			"sweephtlc: swap hash validated for %v",
		},
	},
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
				make([]byte, 20), lnd.ChainParams,
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

			// Drain signer requests to avoid blocking.
			go func() {
				select {
				case <-lnd.SignOutputRawChannel:

				case <-ctx.Done():
				}
			}()

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
				lnd.Signer,
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
func (f *formatLogger) Tracef(format string, params ...interface{}) {
	f.record(format)
	f.Logger.Tracef(format, params...)
}

// Debugf logs a debug message and records its format.
func (f *formatLogger) Debugf(format string, params ...interface{}) {
	f.record(format)
	f.Logger.Debugf(format, params...)
}

// Infof logs an info message and records its format.
func (f *formatLogger) Infof(format string, params ...interface{}) {
	f.record(format)
	f.Logger.Infof(format, params...)
}

// Warnf logs a warning and records its format.
func (f *formatLogger) Warnf(format string, params ...interface{}) {
	f.record(format)
	f.Logger.Warnf(format, params...)
}

// Errorf logs an error and records its format.
func (f *formatLogger) Errorf(format string, params ...interface{}) {
	f.record(format)
	f.Logger.Errorf(format, params...)
}

// Criticalf logs a critical message and records its format.
func (f *formatLogger) Criticalf(format string, params ...interface{}) {
	f.record(format)
	f.Logger.Criticalf(format, params...)
}
