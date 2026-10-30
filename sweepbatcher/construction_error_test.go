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
		presigned        bool
		feeBumping       bool
	}{
		{
			name:       "fee and dust arithmetic",
			value:      200,
			wantError:  "failed to construct tx",
			feeBumping: true,
		},
		{
			name:             "transient wallet fee lookup",
			value:            1_000_000,
			wantError:        "failed to get min relay fee",
			failMinRelayOnce: true,
		},
		{
			name:             "presigned wallet fee lookup",
			value:            1_000_000,
			wantError:        "failed to get minRelayFeeRate",
			failMinRelayOnce: true,
			presigned:        true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testConstructionErrorKeepsBatcherRunning(
				t, tc.value, tc.wantError, tc.failMinRelayOnce,
				tc.presigned, tc.feeBumping,
			)
		})
	}
}

func testConstructionErrorKeepsBatcherRunning(t *testing.T,
	value btcutil.Amount, wantError string, failMinRelayOnce,
	presigned, feeBumping bool) {

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
	helper := newMockPresignedHelper()
	makeSweep := func(id byte, value btcutil.Amount) *SweepRequest {
		hash := lntypes.Hash{id}
		outpoint := wire.OutPoint{
			Hash:  chainhash.Hash{id},
			Index: 1,
		}
		timeout := int32(900)
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
			IsPresigned:          presigned && id == 1,
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
	allowPublish := make(chan struct{})
	options := []BatcherOption{
		WithPresignedHelper(helper),
		WithInitialDelay(func(context.Context, int, btcutil.Amount,
			bool) (time.Duration, error) {

			select {
			case <-allowPublish:
				return 0, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
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
	}
	if !feeBumping {
		options = append(options, WithCustomFeeRate(
			func(context.Context, lntypes.Hash, wire.OutPoint) (
				chainfee.SatPerKWeight, error) {

				return 10_000, nil
			},
		))
	}
	batcher := NewBatcher(
		wallet, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		NewStoreMock(), fetcher, options...,
	)

	runErr := make(chan error, 1)
	go func() {
		runErr <- batcher.Run(ctx)
	}()

	firstSweep := makeSweep(1, value)
	if presigned {
		op := firstSweep.Inputs[0].Outpoint
		helper.SetOutpointOnline(op, true)
		require.NoError(t, batcher.PresignSweepsGroup(
			ctx, firstSweep.Inputs, 900, destAddr, nil,
		))
	}
	require.NoError(t, batcher.AddSweep(ctx, firstSweep))
	firstBatch := getOnlyBatch(t, ctx, batcher)
	if failingWallet != nil {
		failingWallet.fail.Store(true)
	}
	close(allowPublish)
	select {
	case err := <-constructErrors:
		require.ErrorContains(t, err, wantError)
	case err := <-runErr:
		t.Fatalf("batcher stopped on construction error: %v", err)
	case <-time.After(test.Timeout):
		t.Fatal("unconstructable batch did not attempt publication")
	}
	if presigned || feeBumping {
		var firstRate chainfee.SatPerKWeight
		if feeBumping {
			firstBatch.testRunInEventLoop(ctx, func() {
				firstRate = firstBatch.rbfCache.FeeRate
			})
		}
		if presigned {
			failingWallet.fail.Store(true)
		}
		require.NoError(t, lnd.NotifyHeight(601))
		select {
		case err := <-constructErrors:
			require.ErrorContains(t, err, wantError)
		case err := <-runErr:
			t.Fatalf("batcher stopped on presigned retry: %v", err)
		case <-time.After(test.Timeout):
			t.Fatal("failed batch did not retry")
		}
		if feeBumping {
			var secondRate chainfee.SatPerKWeight
			firstBatch.testRunInEventLoop(ctx, func() {
				secondRate = firstBatch.rbfCache.FeeRate
			})
			require.Equal(t, firstRate, secondRate)
		}
	}

	// A healthy sweep with the same timeout must be assigned to another
	// batch while the first one cannot construct a transaction.
	secondSweep := makeSweep(2, 1_000_000)
	require.NoError(t, batcher.AddSweep(ctx, secondSweep))
	require.Len(t, getBatches(ctx, batcher), 2)
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
