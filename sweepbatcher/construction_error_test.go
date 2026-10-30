package sweepbatcher

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

type firstMinRelayFailure struct {
	lndclient.WalletKitClient
	fail atomic.Bool
}

func (w *firstMinRelayFailure) MinRelayFee(ctx context.Context) (
	chainfee.SatPerKWeight, error) {

	if w.fail.Swap(false) {
		return 0, errors.New("min relay fee unavailable")
	}

	return w.WalletKitClient.MinRelayFee(ctx)
}

// TestConstructionErrorKeepsBatcherRunning verifies that an unconstructable
// batch does not stop sweeps belonging to other swaps.
func TestConstructionErrorKeepsBatcherRunning(t *testing.T) {
	testCases := []struct {
		name             string
		value            btcutil.Amount
		wantError        string
		failMinRelayOnce bool
	}{
		{
			name:      "fee and dust arithmetic",
			value:     200,
			wantError: "failed to construct tx",
		},
		{
			name:             "transient wallet fee lookup",
			value:            1_000_000,
			wantError:        "failed to get min relay fee",
			failMinRelayOnce: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testConstructionErrorKeepsBatcherRunning(
				t, tc.value, tc.wantError, tc.failMinRelayOnce,
			)
		})
	}
}

func testConstructionErrorKeepsBatcherRunning(t *testing.T,
	value btcutil.Amount, wantError string, failMinRelayOnce bool) {

	lnd := test.NewMockLnd()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var wallet lndclient.WalletKitClient = lnd.WalletKit
	var failingWallet *firstMinRelayFailure
	if failMinRelayOnce {
		failingWallet = &firstMinRelayFailure{
			WalletKitClient: wallet,
		}
		wallet = failingWallet
	}

	fetcher := &sweepFetcherMock{
		store: make(map[wire.OutPoint]*SweepInfo),
	}
	makeSweep := func(id byte, value btcutil.Amount) *SweepRequest {
		hash := lntypes.Hash{id}
		outpoint := wire.OutPoint{
			Hash:  chainhash.Hash{id},
			Index: 1,
		}
		timeout := int32(900)
		if id == 2 {
			timeout = 1_500
		}
		contract := &loopdb.SwapContract{
			CltvExpiry:      timeout,
			AmountRequested: 1_000_000,
			ProtocolVersion: loopdb.ProtocolVersionMuSig2,
			HtlcKeys:        htlcKeys,
			Preimage:        lntypes.Preimage{id},
		}
		htlc, err := utils.GetHtlc(hash, contract, lnd.ChainParams)
		require.NoError(t, err)
		fetcher.setSweep(outpoint, &SweepInfo{
			ConfTarget:           confTarget,
			Timeout:              timeout,
			InitiationHeight:     500,
			HTLC:                 *htlc,
			Preimage:             lntypes.Preimage{id},
			HTLCKeys:             htlcKeys,
			HTLCSuccessEstimator: htlc.AddSuccessToEstimator,
			ProtocolVersion:      loopdb.ProtocolVersionMuSig2,
			IsExternalAddr:       true,
			DestAddr:             destAddr,
		})

		return &SweepRequest{
			SwapHash: hash,
			Inputs: []Input{{
				Outpoint: outpoint,
				Value:    value,
			}},
			Notifier: &SpendNotifier{
				SpendChan:    make(chan *SpendDetail, 1),
				SpendErrChan: make(chan error, 1),
				QuitChan:     make(chan bool),
			},
		}
	}

	// Registration uses unbuffered channels in the mock.
	go func() {
		for {
			select {
			case <-lnd.RegisterSpendChannel:
			case <-lnd.RegisterConfChannel:
			case <-ctx.Done():
				return
			}
		}
	}()

	constructErrors := make(chan error, 1)
	batcher := NewBatcher(
		wallet, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		NewStoreMock(), fetcher,
		WithCustomFeeRate(func(context.Context, lntypes.Hash,
			wire.OutPoint) (chainfee.SatPerKWeight, error) {

			return 10_000, nil
		}),
		WithInitialDelay(func(context.Context, int, btcutil.Amount,
			bool) (time.Duration, error) {

			return 100 * time.Millisecond, nil
		}),
		WithPublishErrorHandler(func(err error, msg string,
			_ btclog.Logger) {

			if msg == "construct error" {
				select {
				case constructErrors <- err:
				default:
				}
			}
		}),
	)

	runErr := make(chan error, 1)
	go func() {
		runErr <- batcher.Run(ctx)
	}()

	require.NoError(t, batcher.AddSweep(ctx, makeSweep(1, value)))
	if failingWallet != nil {
		failingWallet.fail.Store(true)
	}
	select {
	case err := <-constructErrors:
		require.ErrorContains(t, err, wantError)
	case err := <-runErr:
		t.Fatalf("batcher stopped on construction error: %v", err)
	case <-time.After(test.Timeout):
		t.Fatal("unconstructable batch did not attempt publication")
	}

	// The external destination and distinct timeout range force separate
	// batches. The second swap must publish while the first remains pending.
	require.NoError(t, batcher.AddSweep(ctx, makeSweep(2, 1_000_000)))
	select {
	case <-lnd.TxPublishChannel:
	case err := <-runErr:
		t.Fatalf("healthy batch was stopped: %v", err)
	case <-time.After(test.Timeout):
		t.Fatal("healthy batch did not publish")
	}

	cancel()
	select {
	case err := <-runErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(test.Timeout):
		t.Fatal("batcher did not stop after cancellation")
	}
}
