package sweepbatcher

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestConstructUnsignedTx verifies that the function constructUnsignedTx
// correctly creates unsigned transactions.
func TestConstructUnsignedTx(t *testing.T) {
	// Prepare the necessary data for test cases.
	op1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1, 1},
		Index: 1,
	}
	op2 := wire.OutPoint{
		Hash:  chainhash.Hash{2, 2, 2},
		Index: 2,
	}

	batchPkScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	p2trAddr := "bcrt1pa38tp2hgjevqv3jcsxeu7v72n0s5a3ck8q2u8r" +
		"k6mm67dv7uk26qq8je7e"
	p2trAddress, err := btcutil.DecodeAddress(p2trAddr, nil)
	require.NoError(t, err)
	p2trPkScript, err := txscript.PayToAddrScript(p2trAddress)
	require.NoError(t, err)

	serializedPubKey := []byte{
		0x02, 0x19, 0x2d, 0x74, 0xd0, 0xcb, 0x94, 0x34, 0x4c, 0x95,
		0x69, 0xc2, 0xe7, 0x79, 0x01, 0x57, 0x3d, 0x8d, 0x79, 0x03,
		0xc3, 0xeb, 0xec, 0x3a, 0x95, 0x77, 0x24, 0x89, 0x5d, 0xca,
		0x52, 0xc6, 0xb4}
	p2pkAddress, err := btcutil.NewAddressPubKey(
		serializedPubKey, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	swapHash := lntypes.Hash{1, 1, 1}

	swapContract := &loopdb.SwapContract{
		CltvExpiry:      222,
		AmountRequested: 2_000_000,
		ProtocolVersion: loopdb.ProtocolVersionMuSig2,
		HtlcKeys:        htlcKeys,
	}

	htlc, err := utils.GetHtlc(
		swapHash, swapContract, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	estimator := htlc.AddSuccessToEstimator

	brokenEstimator := func(*input.TxWeightEstimator) error {
		return fmt.Errorf("weight estimator test failure")
	}

	cases := []struct {
		name             string
		sweeps           []sweep
		address          btcutil.Address
		currentHeight    int32
		feeRate          chainfee.SatPerKWeight
		wantErr          string
		wantTx           *wire.MsgTx
		wantWeight       lntypes.WeightUnit
		wantFeeForWeight btcutil.Amount
		wantFee          btcutil.Amount
	}{
		{
			name:    "no sweeps error",
			wantErr: "no sweeps in batch",
		},

		{
			name: "two coop sweeps",
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
				},
				{
					outpoint: op2,
					value:    2_000_000,
				},
			},
			address:       destAddr,
			currentHeight: 800_000,
			feeRate:       1000,
			wantTx: &wire.MsgTx{
				Version:  2,
				LockTime: 800_000,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
					},
					{
						PreviousOutPoint: op2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
			},
			wantWeight:       626,
			wantFeeForWeight: 626,
			wantFee:          626,
		},

		{
			name: "p2tr destination address",
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
				},
				{
					outpoint: op2,
					value:    2_000_000,
				},
			},
			address:       p2trAddress,
			currentHeight: 800_000,
			feeRate:       1000,
			wantTx: &wire.MsgTx{
				Version:  2,
				LockTime: 800_000,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
					},
					{
						PreviousOutPoint: op2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999326,
						PkScript: p2trPkScript,
					},
				},
			},
			wantWeight:       674,
			wantFeeForWeight: 674,
			wantFee:          674,
		},

		{
			name: "unknown kind of address",
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
				},
				{
					outpoint: op2,
					value:    2_000_000,
				},
			},
			address: nil,
			wantErr: "unsupported address type",
		},

		{
			name: "pay-to-pubkey address",
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
				},
				{
					outpoint: op2,
					value:    2_000_000,
				},
			},
			address: p2pkAddress,
			wantErr: "unknown address type",
		},

		{
			name: "fee more than 20% clamped",
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
				},
				{
					outpoint: op2,
					value:    2_000_000,
				},
			},
			address:       destAddr,
			currentHeight: 800_000,
			feeRate:       1_000_000,
			wantTx: &wire.MsgTx{
				Version:  2,
				LockTime: 800_000,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
					},
					{
						PreviousOutPoint: op2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2400000,
						PkScript: batchPkScript,
					},
				},
			},
			wantWeight:       626,
			wantFeeForWeight: 626_000,
			wantFee:          600_000,
		},

		{
			name: "coop and noncoop",
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
				},
				{
					outpoint:             op2,
					value:                2_000_000,
					nonCoopHint:          true,
					htlc:                 *htlc,
					htlcSuccessEstimator: estimator,
				},
			},
			address:       destAddr,
			currentHeight: 800_000,
			feeRate:       1000,
			wantTx: &wire.MsgTx{
				Version:  2,
				LockTime: 800_000,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         1,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999211,
						PkScript: batchPkScript,
					},
				},
			},
			wantWeight:       789,
			wantFeeForWeight: 789,
			wantFee:          789,
		},

		{
			name: "weight estimator fails",
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
				},
				{
					outpoint:             op2,
					value:                2_000_000,
					nonCoopHint:          true,
					htlc:                 *htlc,
					htlcSuccessEstimator: brokenEstimator,
				},
			},
			address:       destAddr,
			currentHeight: 800_000,
			feeRate:       1000,
			wantErr: "sweep.htlcSuccessEstimator failed: " +
				"weight estimator test failure",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tx, weight, feeForW, fee, err := constructUnsignedTx(
				tc.sweeps, tc.address, tc.currentHeight,
				tc.feeRate,
			)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantTx, tx)
				require.Equal(t, tc.wantWeight, weight)
				require.Equal(t, tc.wantFeeForWeight, feeForW)
				require.Equal(t, tc.wantFee, fee)
			}
		})
	}
}
