package sweepbatcher

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// mockPresigner is an implementation of Presigner used in TestPresign.
type mockPresigner struct {
	// outputs collects outputs of presigned transactions.
	outputs []btcutil.Amount

	// lockTimes collects LockTime's of presigned transactions.
	lockTimes []uint32

	// failAt is optional index of a call at which it fails, 1 based.
	failAt int
}

// Presign memorizes the value of the output and fails if the number of
// calls previously made is failAt.
func (p *mockPresigner) Presign(ctx context.Context, tx *wire.MsgTx,
	inputAmt btcutil.Amount) error {

	if len(p.outputs)+1 == p.failAt {
		return fmt.Errorf("test error in Presign")
	}

	p.outputs = append(p.outputs, btcutil.Amount(tx.TxOut[0].Value))
	p.lockTimes = append(p.lockTimes, tx.LockTime)

	return nil
}

// TestPresign checks that function presign presigns correct set of transactions
// and handles edge cases properly.
func TestPresign(t *testing.T) {
	// Prepare the necessary data for test cases.
	op1 := wire.OutPoint{
		Hash:  chainhash.Hash{1, 1, 1},
		Index: 1,
	}
	op2 := wire.OutPoint{
		Hash:  chainhash.Hash{2, 2, 2},
		Index: 2,
	}

	ctx := context.Background()

	cases := []struct {
		name             string
		presigner        presigner
		sweeps           []sweep
		destAddr         btcutil.Address
		nextBlockFeeRate chainfee.SatPerKWeight
		wantErr          string
		wantOutputs      []btcutil.Amount
		wantLockTimes    []uint32
	}{
		{
			name: "error: no presigner",
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
					timeout:  1000,
				},
			},
			destAddr:         destAddr,
			nextBlockFeeRate: chainfee.FeePerKwFloor,
			wantErr:          "presigner is not installed",
		},

		{
			name:             "error: no sweeps",
			presigner:        &mockPresigner{},
			destAddr:         destAddr,
			nextBlockFeeRate: chainfee.FeePerKwFloor,
			wantErr:          "there are no sweeps",
		},

		{
			name:      "error: no destAddr",
			presigner: &mockPresigner{},
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
					timeout:  1000,
				},
			},
			nextBlockFeeRate: chainfee.FeePerKwFloor,
			wantErr:          "unsupported address type <nil>",
		},

		{
			name:      "error: zero nextBlockFeeRate",
			presigner: &mockPresigner{},
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
					timeout:  1000,
				},
				{
					outpoint: op2,
					value:    2_000_000,
					timeout:  1000,
				},
			},
			destAddr: destAddr,
			wantErr:  "nextBlockFeeRate is not set",
		},

		{
			name:      "error: timeout is not set",
			presigner: &mockPresigner{},
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
			destAddr:         destAddr,
			nextBlockFeeRate: chainfee.FeePerKwFloor,
			wantErr:          "timeout is invalid: 0",
		},

		{
			name:      "two coop sweeps",
			presigner: &mockPresigner{},
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
					timeout:  1000,
				},
				{
					outpoint: op2,
					value:    2_000_000,
					timeout:  1100,
				},
			},
			destAddr:         destAddr,
			nextBlockFeeRate: chainfee.FeePerKwFloor,
			wantOutputs: []btcutil.Amount{
				2999842, 2999811, 2999773, 2999728, 2999674,
				2999609, 2999530, 2999436, 2999324, 2999189,
				2999026, 2998832, 2998598, 2998318, 2997982,
				2997578, 2997093, 2996512, 2995815, 2994978,
				2993974, 2992769, 2991323, 2989588, 2987506,
				2985007, 2982008, 2978410, 2974092, 2968910,
				2962692, 2955231, 2946277, 2935533, 2922639,
				2907167, 2888601, 2866321, 2839585, 2807502,
				2769002, 2722803,
			},
			wantLockTimes: []uint32{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 950, 950, 950,
				950, 950, 950, 950, 950, 950, 950, 950, 950,
				950, 950, 950, 950,
			},
		},

		{
			name:      "high current feerate => locktime later",
			presigner: &mockPresigner{},
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
					timeout:  1000,
				},
				{
					outpoint: op2,
					value:    2_000_000,
					timeout:  1000,
				},
			},
			destAddr:         destAddr,
			nextBlockFeeRate: 50 * chainfee.FeePerKwFloor,
			wantOutputs: []btcutil.Amount{
				2999842, 2999811, 2999773, 2999728, 2999674,
				2999609, 2999530, 2999436, 2999324, 2999189,
				2999026, 2998832, 2998598, 2998318, 2997982,
				2997578, 2997093, 2996512, 2995815, 2994978,
				2993974, 2992769, 2991323, 2989588, 2987506,
				2985007, 2982008, 2978410, 2974092, 2968910,
				2962692, 2955231, 2946277, 2935533, 2922639,
				2907167, 2888601, 2866321, 2839585, 2807502,
				2769002, 2722803,
			},
			wantLockTimes: []uint32{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 950, 950, 950, 950, 950, 950, 950,
			},
		},

		{
			name:      "small amount => fewer steps until clamped",
			presigner: &mockPresigner{},
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000,
					timeout:  1000,
				},
				{
					outpoint: op2,
					value:    2_000,
					timeout:  1000,
				},
			},
			destAddr:         destAddr,
			nextBlockFeeRate: chainfee.FeePerKwFloor,
			wantOutputs: []btcutil.Amount{
				2842, 2811, 2773, 2728, 2674, 2609, 2530, 2436,
				2400,
			},
			wantLockTimes: []uint32{
				0, 0, 0, 0, 0, 0, 0, 0, 0,
			},
		},

		{
			name: "third signing fails",
			presigner: &mockPresigner{
				failAt: 3,
			},
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000,
					timeout:  1000,
				},
				{
					outpoint: op2,
					value:    2_000,
					timeout:  1000,
				},
			},
			destAddr:         destAddr,
			nextBlockFeeRate: chainfee.FeePerKwFloor,
			wantErr:          "for feeRate 363 sat/kw",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := presign(
				ctx, tc.presigner, tc.destAddr, tc.sweeps,
				tc.nextBlockFeeRate,
			)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				p := tc.presigner.(*mockPresigner)
				require.Equal(t, tc.wantOutputs, p.outputs)
				require.Equal(t, tc.wantLockTimes, p.lockTimes)
			}
		})
	}
}

// TestCheckSignedTx tests that function CheckSignedTx checks all the criteria
// of PresignedHelper.SignTx correctly.
func TestCheckSignedTx(t *testing.T) {
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

	cases := []struct {
		name        string
		unsignedTx  *wire.MsgTx
		signedTx    *wire.MsgTx
		inputAmt    btcutil.Amount
		minRelayFee chainfee.SatPerKWeight
		wantErr     string
	}{
		{
			name: "success",
			unsignedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 800_000,
			},
			signedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 799_999,
			},
			inputAmt:    3_000_000,
			minRelayFee: 253,
			wantErr:     "",
		},

		{
			name: "bad locktime",
			unsignedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 800_000,
			},
			signedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 800_001,
			},
			inputAmt:    3_000_000,
			minRelayFee: 253,
			wantErr:     "locktime",
		},

		{
			name: "bad version",
			unsignedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 800_000,
			},
			signedTx: &wire.MsgTx{
				Version: 3,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 799_999,
			},
			inputAmt:    3_000_000,
			minRelayFee: 253,
			wantErr:     "version",
		},

		{
			name: "missing input",
			unsignedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 800_000,
			},
			signedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 799_999,
			},
			inputAmt:    3_000_000,
			minRelayFee: 253,
			wantErr:     "is missing in signed tx",
		},

		{
			name: "extra input",
			unsignedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 800_000,
			},
			signedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 799_999,
			},
			inputAmt:    3_000_000,
			minRelayFee: 253,
			wantErr:     "is new in signed tx",
		},

		{
			name: "mismatch of sequence numbers",
			unsignedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 800_000,
			},
			signedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
					{
						PreviousOutPoint: op1,
						Sequence:         3,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 799_999,
			},
			inputAmt:    3_000_000,
			minRelayFee: 253,
			wantErr:     "sequence mismatch",
		},

		{
			name: "extra output in unsignedTx",
			unsignedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 800_000,
			},
			signedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 799_999,
			},
			inputAmt:    3_000_000,
			minRelayFee: 253,
			wantErr:     "unsigned tx has 2 outputs, want 1",
		},

		{
			name: "extra output in signedTx",
			unsignedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 800_000,
			},
			signedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 799_999,
			},
			inputAmt:    3_000_000,
			minRelayFee: 253,
			wantErr:     "the signed tx has 2 outputs, want 1",
		},

		{
			name: "mismatch of output pk_script",
			unsignedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 800_000,
			},
			signedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript[1:],
					},
				},
				LockTime: 799_999,
			},
			inputAmt:    3_000_000,
			minRelayFee: 253,
			wantErr:     "mismatch of output pkScript",
		},

		{
			name: "too low feerate in signedTx",
			unsignedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 800_000,
			},
			signedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness: wire.TxWitness{
							[]byte("test"),
						},
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
				LockTime: 799_999,
			},
			inputAmt:    3_000_000,
			minRelayFee: 250_000,
			wantErr:     "is lower than minRelayFee",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckSignedTx(
				tc.unsignedTx, tc.signedTx, tc.inputAmt,
				tc.minRelayFee,
			)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
