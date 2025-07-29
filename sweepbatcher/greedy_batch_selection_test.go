package sweepbatcher

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// Useful constants for tests.
const (
	lowFeeRate    = chainfee.FeePerKwFloor
	mediumFeeRate = lowFeeRate + 200
	highFeeRate   = chainfee.SatPerKWeight(30000)

	coopInputWeight       = lntypes.WeightUnit(230)
	batchOutputWeight     = lntypes.WeightUnit(343)
	nonCoopInputWeight    = lntypes.WeightUnit(393)
	nonCoopPenalty        = nonCoopInputWeight - coopInputWeight
	coopNewBatchWeight    = lntypes.WeightUnit(444)
	nonCoopNewBatchWeight = coopNewBatchWeight + nonCoopPenalty
	changeOutputWeight    = lntypes.WeightUnit(input.P2TROutputSize)

	// p2pkhDiscount is weight discount P2PKH output has over P2TR output.
	p2pkhDiscount = lntypes.WeightUnit(
		input.P2TROutputSize-input.P2PKHOutputSize,
	) * 4

	coopTwoSweepBatchWeight          = coopNewBatchWeight + coopInputWeight
	coopSingleSweepChangeBatchWeight = coopInputWeight + batchOutputWeight + changeOutputWeight
	coopDoubleSweepChangeBatchWeight = 2*coopInputWeight + batchOutputWeight + changeOutputWeight
	nonCoopTwoSweepBatchWeight       = coopTwoSweepBatchWeight + 2*nonCoopPenalty
	v2v3BatchWeight                  = nonCoopTwoSweepBatchWeight - 25
	mixedTwoSweepBatchWeight         = coopTwoSweepBatchWeight + nonCoopPenalty
)

// testHtlcV2SuccessEstimator adds weight of non-cooperative input to estimator
// using HTLC v2.
func testHtlcV2SuccessEstimator(estimator *input.TxWeightEstimator) error {
	swapHash := lntypes.Hash{1, 1, 1}
	htlc, err := swap.NewHtlcV2(
		111, htlcKeys.SenderScriptKey, htlcKeys.ReceiverScriptKey,
		swapHash, &chaincfg.RegressionNetParams,
	)
	if err != nil {
		return err
	}
	return htlc.AddSuccessToEstimator(estimator)
}

// testHtlcV3SuccessEstimator adds weight of non-cooperative input to estimator
// using HTLC v3.
func testHtlcV3SuccessEstimator(estimator *input.TxWeightEstimator) error {
	swapHash := lntypes.Hash{1, 1, 1}
	htlc, err := swap.NewHtlcV3(
		input.MuSig2Version100RC2, 111,
		htlcKeys.SenderInternalPubKey, htlcKeys.ReceiverInternalPubKey,
		htlcKeys.SenderScriptKey, htlcKeys.ReceiverScriptKey, swapHash,
		&chaincfg.RegressionNetParams,
	)
	if err != nil {
		return err
	}
	return htlc.AddSuccessToEstimator(estimator)
}

// TestEstimateSweepFeeIncrement tests that weight and fee estimations work
// correctly for a sweep and one sweep batch.
func TestEstimateSweepFeeIncrement(t *testing.T) {
	// Useful variables reused in test cases.
	se3 := testHtlcV3SuccessEstimator
	trAddr := (*btcutil.AddressTaproot)(nil)
	p2pkhAddr := (*btcutil.AddressPubKeyHash)(nil)

	outpoint1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1, 1},
		Index: 1,
	}
	outpoint2 := wire.OutPoint{
		Hash:  chainhash.Hash{2, 2, 2},
		Index: 2,
	}

	cases := []struct {
		name                   string
		sweeps                 []*sweep
		wantSweepFeeDetails    feeDetails
		wantNewBatchFeeDetails feeDetails
	}{
		{
			name: "regular",
			sweeps: []*sweep{
				{
					minFeeRate:           lowFeeRate,
					htlcSuccessEstimator: se3,
				},
			},
			wantSweepFeeDetails: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopInputWeight,
			},
			wantNewBatchFeeDetails: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopNewBatchWeight,
			},
		},

		{
			name: "high fee rate",
			sweeps: []*sweep{
				{
					minFeeRate:           highFeeRate,
					htlcSuccessEstimator: se3,
				},
			},
			wantSweepFeeDetails: feeDetails{
				FeeRate: highFeeRate,
				Weight:  coopInputWeight,
			},
			wantNewBatchFeeDetails: feeDetails{
				FeeRate: highFeeRate,
				Weight:  coopNewBatchWeight,
			},
		},

		{
			name: "isExternalAddr taproot",
			sweeps: []*sweep{
				{
					minFeeRate:           lowFeeRate,
					htlcSuccessEstimator: se3,
					isExternalAddr:       true,
					destAddr:             trAddr,
				},
			},
			wantSweepFeeDetails: feeDetails{
				FeeRate:        lowFeeRate,
				Weight:         coopInputWeight,
				IsExternalAddr: true,
			},
			wantNewBatchFeeDetails: feeDetails{
				FeeRate:        lowFeeRate,
				Weight:         coopNewBatchWeight,
				IsExternalAddr: true,
			},
		},

		{
			name: "isExternalAddr P2PKH",
			sweeps: []*sweep{
				{
					minFeeRate:           lowFeeRate,
					htlcSuccessEstimator: se3,
					isExternalAddr:       true,
					destAddr:             p2pkhAddr,
				},
			},
			wantSweepFeeDetails: feeDetails{
				FeeRate:        lowFeeRate,
				Weight:         coopInputWeight,
				IsExternalAddr: true,
			},
			wantNewBatchFeeDetails: feeDetails{
				FeeRate: lowFeeRate,
				Weight: coopNewBatchWeight -
					p2pkhDiscount,
				IsExternalAddr: true,
			},
		},

		{
			name: "non-coop",
			sweeps: []*sweep{
				{
					minFeeRate:           lowFeeRate,
					htlcSuccessEstimator: se3,
					nonCoopHint:          true,
				},
			},
			wantSweepFeeDetails: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  nonCoopInputWeight,
			},
			wantNewBatchFeeDetails: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  nonCoopNewBatchWeight,
			},
		},

		{
			name: "coop-failed",
			sweeps: []*sweep{
				{
					minFeeRate:           lowFeeRate,
					htlcSuccessEstimator: se3,
					coopFailed:           true,
				},
			},
			wantSweepFeeDetails: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  nonCoopInputWeight,
			},
			wantNewBatchFeeDetails: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  nonCoopNewBatchWeight,
			},
		},

		{
			name: "two sweeps",
			sweeps: []*sweep{
				{
					outpoint:             outpoint1,
					minFeeRate:           lowFeeRate,
					htlcSuccessEstimator: se3,
				},
				{
					outpoint:             outpoint2,
					minFeeRate:           highFeeRate,
					htlcSuccessEstimator: se3,
				},
			},
			wantSweepFeeDetails: feeDetails{
				FeeRate: highFeeRate,
				Weight:  coopInputWeight * 2,
			},
			wantNewBatchFeeDetails: feeDetails{
				FeeRate: highFeeRate,
				Weight:  coopNewBatchWeight + coopInputWeight,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSweepFeeDetails, gotNewBatchFeeDetails, err :=
				estimateSweepFeeIncrement(tc.sweeps)
			require.NoError(t, err)
			require.Equal(
				t, tc.wantSweepFeeDetails, gotSweepFeeDetails,
			)
			require.Equal(
				t, tc.wantNewBatchFeeDetails,
				gotNewBatchFeeDetails,
			)
		})
	}
}

// TestEstimateBatchWeight tests that weight and fee estimations work correctly
// for batches.
func TestEstimateBatchWeight(t *testing.T) {
	// Useful variables reused in test cases.
	outpoint1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1, 1},
		Index: 1,
	}
	outpoint2 := wire.OutPoint{
		Hash:  chainhash.Hash{2, 2, 2},
		Index: 2,
	}
	se2 := testHtlcV2SuccessEstimator
	se3 := testHtlcV3SuccessEstimator
	trAddr := (*btcutil.AddressTaproot)(nil)

	changeAddr := "bc1pdx9ggvtjjcpaqfqk375qhdmzx9xu8dcu7w94lqfcxhh0rj" +
		"lwyyeq5ryn6r"
	changeAddress, err := btcutil.DecodeAddress(changeAddr, nil)
	require.NoError(t, err)
	changePkscript, err := txscript.PayToAddrScript(changeAddress)
	require.NoError(t, err)

	cases := []struct {
		name                string
		batch               *batch
		wantBatchFeeDetails feeDetails
	}{
		{
			name: "one sweep regular batch",
			batch: &batch{
				id: 1,
				rbfCache: rbfCache{
					FeeRate: lowFeeRate,
				},
				sweeps: map[wire.OutPoint]sweep{
					outpoint1: {
						htlcSuccessEstimator: se3,
					},
				},
			},
			wantBatchFeeDetails: feeDetails{
				BatchId: 1,
				FeeRate: lowFeeRate,
				Weight:  coopNewBatchWeight,
			},
		},

		{
			name: "one sweep regular batch with change",
			batch: &batch{
				id: 1,
				rbfCache: rbfCache{
					FeeRate: lowFeeRate,
				},
				sweeps: map[wire.OutPoint]sweep{
					outpoint1: {
						htlcSuccessEstimator: se3,
						change: &wire.TxOut{
							PkScript: changePkscript,
						},
					},
				},
			},
			wantBatchFeeDetails: feeDetails{
				BatchId: 1,
				FeeRate: lowFeeRate,
				Weight:  coopSingleSweepChangeBatchWeight,
			},
		},

		{
			name: "two sweeps regular batch with identical change",
			batch: &batch{
				id: 1,
				rbfCache: rbfCache{
					FeeRate: lowFeeRate,
				},
				sweeps: map[wire.OutPoint]sweep{
					outpoint1: {
						htlcSuccessEstimator: se3,
						change: &wire.TxOut{
							PkScript: changePkscript,
						},
					},
					outpoint2: {
						htlcSuccessEstimator: se3,
						change: &wire.TxOut{
							PkScript: changePkscript,
						},
					},
				},
			},
			wantBatchFeeDetails: feeDetails{
				BatchId: 1,
				FeeRate: lowFeeRate,
				Weight:  coopDoubleSweepChangeBatchWeight,
			},
		},

		{
			name: "two sweeps regular batch",
			batch: &batch{
				id: 1,
				rbfCache: rbfCache{
					FeeRate: lowFeeRate,
				},
				sweeps: map[wire.OutPoint]sweep{
					outpoint1: {
						htlcSuccessEstimator: se3,
					},
					outpoint2: {
						htlcSuccessEstimator: se3,
					},
				},
			},
			wantBatchFeeDetails: feeDetails{
				BatchId: 1,
				FeeRate: lowFeeRate,
				Weight:  coopTwoSweepBatchWeight,
			},
		},

		{
			name: "v2 and v3 sweeps",
			batch: &batch{
				id: 1,
				rbfCache: rbfCache{
					FeeRate: lowFeeRate,
				},
				sweeps: map[wire.OutPoint]sweep{
					outpoint1: {
						htlcSuccessEstimator: se2,
					},
					outpoint2: {
						htlcSuccessEstimator: se3,
					},
				},
			},
			wantBatchFeeDetails: feeDetails{
				BatchId: 1,
				FeeRate: lowFeeRate,
				Weight:  coopTwoSweepBatchWeight,
			},
		},

		{
			name: "v2 and v3 sweeps noncoop",
			batch: &batch{
				id: 1,
				rbfCache: rbfCache{
					FeeRate: lowFeeRate,
				},
				sweeps: map[wire.OutPoint]sweep{
					outpoint1: {
						htlcSuccessEstimator: se2,
						nonCoopHint:          true,
					},
					outpoint2: {
						htlcSuccessEstimator: se3,
						nonCoopHint:          true,
					},
				},
			},
			wantBatchFeeDetails: feeDetails{
				BatchId: 1,
				FeeRate: lowFeeRate,
				Weight:  v2v3BatchWeight,
			},
		},

		{
			name: "high fee rate",
			batch: &batch{
				id: 1,
				rbfCache: rbfCache{
					FeeRate: highFeeRate,
				},
				sweeps: map[wire.OutPoint]sweep{
					outpoint1: {
						htlcSuccessEstimator: se3,
					},
				},
			},
			wantBatchFeeDetails: feeDetails{
				BatchId: 1,
				FeeRate: highFeeRate,
				Weight:  coopNewBatchWeight,
			},
		},

		{
			name: "non-coop",
			batch: &batch{
				id: 1,
				rbfCache: rbfCache{
					FeeRate: lowFeeRate,
				},
				sweeps: map[wire.OutPoint]sweep{
					outpoint1: {
						htlcSuccessEstimator: se3,
					},
					outpoint2: {
						htlcSuccessEstimator: se3,
						nonCoopHint:          true,
					},
				},
			},
			wantBatchFeeDetails: feeDetails{
				BatchId: 1,
				FeeRate: lowFeeRate,
				Weight:  mixedTwoSweepBatchWeight,
			},
		},

		{
			name: "coop-failed",
			batch: &batch{
				id: 1,
				rbfCache: rbfCache{
					FeeRate: lowFeeRate,
				},
				sweeps: map[wire.OutPoint]sweep{
					outpoint1: {
						htlcSuccessEstimator: se3,
					},
					outpoint2: {
						htlcSuccessEstimator: se3,
						coopFailed:           true,
					},
				},
			},
			wantBatchFeeDetails: feeDetails{
				BatchId: 1,
				FeeRate: lowFeeRate,
				Weight:  mixedTwoSweepBatchWeight,
			},
		},

		{
			name: "isExternalAddr",
			batch: &batch{
				id: 1,
				rbfCache: rbfCache{
					FeeRate: lowFeeRate,
				},
				sweeps: map[wire.OutPoint]sweep{
					outpoint1: {
						htlcSuccessEstimator: se3,
						isExternalAddr:       true,
						destAddr:             trAddr,
					},
				},
			},
			wantBatchFeeDetails: feeDetails{
				BatchId:        1,
				FeeRate:        lowFeeRate,
				Weight:         coopNewBatchWeight,
				IsExternalAddr: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBatchFeeDetails, err := estimateBatchWeight(tc.batch)
			require.NoError(t, err)
			require.Equal(
				t, tc.wantBatchFeeDetails, gotBatchFeeDetails,
			)
		})
	}
}

// TestSelectBatches tests greedy batch selection algorithm.
func TestSelectBatches(t *testing.T) {
	cases := []struct {
		name                 string
		batches              []feeDetails
		sweep, oneSweepBatch feeDetails
		wantBestBatchesIds   []int32
	}{
		{
			name:    "no existing batches",
			batches: []feeDetails{},
			sweep: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{newBatchSignal},
		},

		{
			name: "low fee sweep, low fee existing batch",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: lowFeeRate,
					Weight:  coopNewBatchWeight,
				},
			},
			sweep: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{1, newBatchSignal},
		},

		{
			name: "low fee sweep, high fee existing batch",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: highFeeRate,
					Weight:  coopNewBatchWeight,
				},
			},
			sweep: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{newBatchSignal, 1},
		},

		{
			name: "low fee sweep, low + high fee existing batches",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: lowFeeRate,
					Weight:  coopNewBatchWeight,
				},
				{
					BatchId: 2,
					FeeRate: highFeeRate,
					Weight:  coopNewBatchWeight,
				},
			},
			sweep: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{1, newBatchSignal, 2},
		},

		{
			name: "high fee sweep, low + high fee existing batches",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: lowFeeRate,
					Weight:  coopNewBatchWeight,
				},
				{
					BatchId: 2,
					FeeRate: highFeeRate,
					Weight:  coopNewBatchWeight,
				},
			},
			sweep: feeDetails{
				FeeRate: highFeeRate,
				Weight:  coopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: highFeeRate,
				Weight:  coopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{2, newBatchSignal, 1},
		},

		{
			name: "high fee noncoop sweep",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: lowFeeRate,
					Weight:  coopNewBatchWeight,
				},
				{
					BatchId: 2,
					FeeRate: highFeeRate,
					Weight:  coopNewBatchWeight,
				},
			},
			sweep: feeDetails{
				FeeRate: highFeeRate,
				Weight:  nonCoopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: highFeeRate,
				Weight:  nonCoopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{2, newBatchSignal, 1},
		},

		{
			name: "high fee noncoop sweep, large batches, mixed",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: lowFeeRate,
					Weight:  10000,
				},
				{
					BatchId: 2,
					FeeRate: highFeeRate,
					Weight:  10000,
				},
			},
			sweep: feeDetails{
				FeeRate: highFeeRate,
				Weight:  nonCoopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: highFeeRate,
				Weight:  nonCoopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{2, newBatchSignal, 1},
		},

		{
			name: "high fee noncoop sweep, high batch noncoop",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: lowFeeRate,
					Weight:  coopNewBatchWeight,
				},
				{
					BatchId: 2,
					FeeRate: highFeeRate,
					Weight:  nonCoopNewBatchWeight,
				},
			},
			sweep: feeDetails{
				FeeRate: highFeeRate,
				Weight:  nonCoopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: highFeeRate,
				Weight:  nonCoopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{2, newBatchSignal, 1},
		},

		{
			name: "low fee noncoop sweep",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: lowFeeRate,
					Weight:  coopNewBatchWeight,
				},
				{
					BatchId: 2,
					FeeRate: highFeeRate,
					Weight:  coopNewBatchWeight,
				},
			},
			sweep: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  nonCoopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  nonCoopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{1, newBatchSignal, 2},
		},

		{
			name: "low fee noncoop sweep, large batches, mixed",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: lowFeeRate,
					Weight:  10000,
				},
				{
					BatchId: 2,
					FeeRate: highFeeRate,
					Weight:  10000,
				},
			},
			sweep: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  nonCoopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  nonCoopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{1, newBatchSignal, 2},
		},

		{
			name: "low fee noncoop sweep, low batch noncoop",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: lowFeeRate,
					Weight:  nonCoopNewBatchWeight,
				},
				{
					BatchId: 2,
					FeeRate: highFeeRate,
					Weight:  coopNewBatchWeight,
				},
			},
			sweep: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  nonCoopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  nonCoopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{1, newBatchSignal, 2},
		},

		{
			name: "external address sweep",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: highFeeRate,
					Weight:  coopNewBatchWeight,
				},
				{
					BatchId: 2,
					FeeRate: highFeeRate,
					Weight:  coopNewBatchWeight,
				},
			},
			sweep: feeDetails{
				FeeRate:        highFeeRate,
				Weight:         coopInputWeight,
				IsExternalAddr: true,
			},
			oneSweepBatch: feeDetails{
				FeeRate:        highFeeRate,
				Weight:         coopNewBatchWeight,
				IsExternalAddr: true,
			},
			wantBestBatchesIds: []int32{newBatchSignal},
		},

		{
			name: "external address batch",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: highFeeRate - 1,
					Weight:  coopNewBatchWeight,
				},
				{
					BatchId:        2,
					FeeRate:        highFeeRate,
					Weight:         coopNewBatchWeight,
					IsExternalAddr: true,
				},
			},
			sweep: feeDetails{
				FeeRate: highFeeRate,
				Weight:  coopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: highFeeRate,
				Weight:  coopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{1, newBatchSignal},
		},

		{
			name: "low fee change sweep, placed in new batch",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: mediumFeeRate,
					Weight:  coopNewBatchWeight,
				},
			},
			sweep: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopInputWeight + changeOutputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{newBatchSignal, 1},
		},

		{
			name: "low fee sweep without change, placed in " +
				"existing batch",
			batches: []feeDetails{
				{
					BatchId: 1,
					FeeRate: mediumFeeRate,
					Weight:  coopNewBatchWeight,
				},
			},
			sweep: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopInputWeight,
			},
			oneSweepBatch: feeDetails{
				FeeRate: lowFeeRate,
				Weight:  coopNewBatchWeight,
			},
			wantBestBatchesIds: []int32{1, newBatchSignal},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBestBatchesIds, err := selectBatches(
				tc.batches, tc.sweep, tc.oneSweepBatch,
			)
			require.NoError(t, err)
			require.Equal(
				t, tc.wantBestBatchesIds, gotBestBatchesIds,
			)
		})
	}
}
