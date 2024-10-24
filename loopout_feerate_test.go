package loop

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// testSweeper is implementation of sweeper.Sweeper for test.
type testSweeper struct {
}

// GetSweepFeeDetails calculates the required tx fee to spend to destAddr. It
// takes a function that is expected to add the weight of the input to the
// weight estimator. It returns also the fee rate and transaction weight.
func (s testSweeper) GetSweepFeeDetails(ctx context.Context,
	addInputEstimate func(*input.TxWeightEstimator) error,
	destAddr btcutil.Address, sweepConfTarget int32,
	label string) (btcutil.Amount, chainfee.SatPerKWeight,
	lntypes.WeightUnit, error) {

	var feeRate chainfee.SatPerKWeight
	switch {
	case sweepConfTarget == 0:
		return 0, 0, 0, fmt.Errorf("zero sweepConfTarget")

	case sweepConfTarget == 1:
		feeRate = 30000

	case sweepConfTarget == 2:
		feeRate = 25000

	case sweepConfTarget == 3:
		feeRate = 20000

	case sweepConfTarget < 10:
		feeRate = 8000

	case sweepConfTarget < 100:
		feeRate = 5000

	case sweepConfTarget < 1000:
		feeRate = 2000

	default:
		feeRate = 250
	}

	// Calculate weight for this tx.
	var weightEstimate input.TxWeightEstimator

	// Add output.
	err := sweep.AddOutputEstimate(&weightEstimate, destAddr)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to add output weight "+
			"estimate: %w", err)
	}

	// Add input.
	err = addInputEstimate(&weightEstimate)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to add input weight "+
			"estimate: %w", err)
	}

	// Find weight.
	weight := weightEstimate.Weight()

	return feeRate.FeeForWeight(weight), feeRate, weight, nil
}

// TestLoopOutSweepFeerateProvider tests that loopOutSweepFeerateProvider
// provides correct fee rate for loop-out swaps.
func TestLoopOutSweepFeerateProvider(t *testing.T) {
	htlcKeys := func() loopdb.HtlcKeys {
		var senderKey, receiverKey [33]byte

		// Generate keys.
		_, senderPubKey := test.CreateKey(1)
		copy(senderKey[:], senderPubKey.SerializeCompressed())
		_, receiverPubKey := test.CreateKey(2)
		copy(receiverKey[:], receiverPubKey.SerializeCompressed())

		return loopdb.HtlcKeys{
			SenderScriptKey:        senderKey,
			ReceiverScriptKey:      receiverKey,
			SenderInternalPubKey:   senderKey,
			ReceiverInternalPubKey: receiverKey,
		}
	}()

	var destAddr *btcutil.AddressTaproot

	swapInvoice := "lntb1230n1pjjszzgpp5j76f03wrkya4sm4gxv6az5nmz5aqsvmn4" +
		"tpguu2sdvdyygedqjgqdq9xyerxcqzzsxqr23ssp5rwzmwtfjmsgranfk8sr" +
		"4p4gcgmvyd42uug8pxteg2mkk23ndvkqs9qyyssq44ruk3ex59cmv4dm6k4v" +
		"0kc6c0gcqjs0gkljfyd6c6uatqa2f67xlx3pcg5tnvcae5p3jju8ra77e87d" +
		"vhhs0jrx53wnc0fq9rkrhmqqelyx7l"

	cases := []struct {
		name            string
		cltvExpiry      int32
		height          int32
		amount          btcutil.Amount
		protocolVersion loopdb.ProtocolVersion
		wantConfTarget  int32
		wantFeeRate     chainfee.SatPerKWeight
		wantError       string
	}{
		{
			name:            "simple case",
			cltvExpiry:      801_000,
			height:          800_900,
			amount:          1_000_000,
			protocolVersion: loopdb.ProtocolVersionMuSig2,
			wantConfTarget:  100,
			wantFeeRate:     2000,
		},
		{
			name:            "zero height",
			cltvExpiry:      801_000,
			height:          0,
			amount:          1_000_000,
			protocolVersion: loopdb.ProtocolVersionMuSig2,
			wantError:       "got zero best block height",
		},
		{
			name:            "huge amount, no proportional fee",
			cltvExpiry:      801_000,
			height:          800_900,
			amount:          100_000_000,
			protocolVersion: loopdb.ProtocolVersionMuSig2,
			wantConfTarget:  100,
			wantFeeRate:     2000,
		},
		{
			name:            "huge amount, no proportional fee, v2",
			cltvExpiry:      801_000,
			height:          800_900,
			amount:          100_000_000,
			protocolVersion: loopdb.ProtocolVersionLoopOutCancel,
			wantConfTarget:  100,
			wantFeeRate:     2000,
		},
		{
			name: "huge amount, no proportional fee, " +
				"capped by urgent fee",
			cltvExpiry:      801_000,
			height:          800_900,
			amount:          200_000_000,
			protocolVersion: loopdb.ProtocolVersionMuSig2,
			wantConfTarget:  100,
			wantFeeRate:     2000,
		},
		{
			name:            "11 blocks until expiry",
			cltvExpiry:      801_000,
			height:          800_989,
			amount:          1_000_000,
			protocolVersion: loopdb.ProtocolVersionMuSig2,
			wantConfTarget:  11,
			wantFeeRate:     5000,
		},
		{
			name:            "10 blocks until expiry",
			cltvExpiry:      801_000,
			height:          800_990,
			amount:          1_000_000,
			protocolVersion: loopdb.ProtocolVersionMuSig2,
			wantConfTarget:  3,
			wantFeeRate:     22000,
		},
		{
			name:            "9 blocks until expiry",
			cltvExpiry:      801_000,
			height:          800_991,
			amount:          1_000_000,
			protocolVersion: loopdb.ProtocolVersionMuSig2,
			wantConfTarget:  3,
			wantFeeRate:     22000,
		},
		{
			name:            "3 blocks until expiry",
			cltvExpiry:      801_000,
			height:          800_997,
			amount:          1_000_000,
			protocolVersion: loopdb.ProtocolVersionMuSig2,
			wantConfTarget:  3,
			wantFeeRate:     22000,
		},
		{
			name:            "2 blocks until expiry",
			cltvExpiry:      801_000,
			height:          800_998,
			amount:          1_000_000,
			protocolVersion: loopdb.ProtocolVersionMuSig2,
			wantConfTarget:  2,
			wantFeeRate:     27500,
		},
		{
			name:            "1 blocks until expiry",
			cltvExpiry:      801_000,
			height:          800_999,
			amount:          1_000_000,
			protocolVersion: loopdb.ProtocolVersionMuSig2,
			wantConfTarget:  1,
			wantFeeRate:     33000,
		},
		{
			name:            "expired",
			cltvExpiry:      801_000,
			height:          801_000,
			amount:          1_000_000,
			protocolVersion: loopdb.ProtocolVersionMuSig2,
			wantConfTarget:  1,
			wantFeeRate:     33000,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store := loopdb.NewStoreMock(t)

			ctx := context.Background()

			swapHash := lntypes.Hash{1, 1, 1}
			swap := &loopdb.LoopOutContract{
				SwapContract: loopdb.SwapContract{
					CltvExpiry:      tc.cltvExpiry,
					AmountRequested: tc.amount,
					ProtocolVersion: tc.protocolVersion,
					HtlcKeys:        htlcKeys,
				},

				DestAddr:        destAddr,
				SwapInvoice:     swapInvoice,
				SweepConfTarget: 100,
			}

			err := store.CreateLoopOut(ctx, swapHash, swap)
			require.NoError(t, err)
			store.AssertLoopOutStored()

			getHeight := func() int32 {
				return tc.height
			}

			p := newLoopOutSweepFeerateProvider(
				testSweeper{}, store,
				&chaincfg.RegressionNetParams, getHeight,
			)

			confTarget, feeRate, err := p.GetConfTargetAndFeeRate(
				ctx, swapHash,
			)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantConfTarget, confTarget)
			require.Equal(t, tc.wantFeeRate, feeRate)
		})
	}
}
