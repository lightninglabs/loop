package sweepbatcher

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestBatchSelectionFiltersSweepMode checks that both selection paths skip
// incompatible batches, without relying on addSweeps to reject them and warn.
func TestBatchSelectionFiltersSweepMode(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, presigned := range []bool{false, true} {
			name := fmt.Sprintf("fallback=%v/incoming_presigned=%v",
				fallback, presigned)
			t.Run(name, func(t *testing.T) {
				testBatchSelectionFiltersSweepMode(
					t, fallback, presigned,
				)
			})
		}
	}
}

func testBatchSelectionFiltersSweepMode(t *testing.T, fallback,
	presigned bool) {

	lnd := test.NewMockLnd()
	helper := newMockPresignedHelper()
	fetcher := &sweepFetcherMock{
		store: make(map[wire.OutPoint]*SweepInfo),
	}
	feeRate := func(context.Context, lntypes.Hash,
		wire.OutPoint) (chainfee.SatPerKWeight, error) {

		return 10_000, nil
	}
	delay := func(context.Context, int, btcutil.Amount,
		bool) (time.Duration, error) {

		return time.Hour, nil
	}
	batcher := NewBatcher(
		lnd.WalletKit, lnd.ChainNotifier, lnd.Signer,
		testMuSig2SignSweep, testVerifySchnorrSig, lnd.ChainParams,
		NewStoreMock(), fetcher, WithCustomFeeRate(feeRate),
		WithPresignedHelper(helper), WithInitialDelay(delay),
	)
	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() {
		runErr <- batcher.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		checkBatcherError(t, <-runErr)
	})

	addSweep := func(id byte, isPresigned, failEstimate bool) wire.OutPoint {
		op := wire.OutPoint{Hash: chainhash.Hash{id}}
		inputs := []Input{{Outpoint: op, Value: 1_000_000}}
		if isPresigned {
			helper.SetOutpointOnline(op, true)
			require.NoError(t, batcher.PresignSweepsGroup(
				ctx, inputs, sweepTimeout, destAddr, nil,
			))
		}
		info, err := helper.FetchSweep(ctx, lntypes.Hash{id}, op)
		require.NoError(t, err)
		if failEstimate {
			// A failed estimate forces the fallback selection path.
			info.NonCoopHint = true
			info.HTLCSuccessEstimator = func(
				*input.TxWeightEstimator) error {

				return errors.New("test weight estimation failure")
			}
		}
		fetcher.setSweep(op, info)
		require.NoError(t, batcher.AddSweep(ctx, &SweepRequest{
			SwapHash: lntypes.Hash{id},
			Inputs:   inputs,
			Notifier: &dummyNotifier,
		}))
		<-lnd.RegisterSpendChannel
		return op
	}

	// With only an incompatible batch available, the old selector always
	// tries it before creating a new batch. Exercise both warning messages.
	op1 := addSweep(1, !presigned, false)
	wrongBatch := getOnlyBatch(t, ctx, batcher)
	recorded := &wrappedLogger{Logger: wrongBatch.log()}
	wrongBatch.setLog(recorded)
	op2 := addSweep(2, presigned, fallback)

	require.Len(t, getBatches(ctx, batcher), 2)
	require.True(t, wrongBatch.sweepExists(op1))
	require.False(t, wrongBatch.sweepExists(op2))
	for _, batch := range getBatches(ctx, batcher) {
		require.Equal(t, 1, batch.numSweeps(ctx))
	}

	// Keep the admission warnings as safeguards. Their absence here proves
	// that selection did not attempt to add to the incompatible batch.
	recorded.mu.Lock()
	warnings := append([]string(nil), recorded.warnMessages...)
	recorded.mu.Unlock()
	require.Empty(t, warnings, "incompatible batch was attempted")
}
