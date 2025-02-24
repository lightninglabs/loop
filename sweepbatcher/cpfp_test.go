package sweepbatcher

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// mockPresigner is an implementation of Presigner used in TestPresign.
type mockPresigner struct {
	// outputs collects outputs of presigned transactions.
	outputs []btcutil.Amount

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
		name        string
		presigner   presigner
		sweeps      []sweep
		destAddr    btcutil.Address
		wantErr     string
		wantOutputs []btcutil.Amount
	}{
		{
			name: "error: no presigner",
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
				},
			},
			destAddr: destAddr,
			wantErr:  "presigner is not installed",
		},

		{
			name:      "error: no sweeps",
			presigner: &mockPresigner{},
			destAddr:  destAddr,
			wantErr:   "there are no sweeps",
		},

		{
			name:      "error: no destAddr",
			presigner: &mockPresigner{},
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
				},
			},
			wantErr: "unsupported address type <nil>",
		},

		{
			name:      "two coop sweeps",
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
			destAddr: destAddr,
			wantOutputs: []btcutil.Amount{
				2999842, 2999763, 2999645, 2999467, 2999200,
				2998800, 2998201, 2997301, 2995952, 2993927,
				2990890, 2986336, 2979503, 2969255, 2953882,
				2930824, 2896235, 2844353, 2766529,
			},
		},

		{
			name:      "small amount => fewer steps until clamped",
			presigner: &mockPresigner{},
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000,
				},
				{
					outpoint: op2,
					value:    2_000,
				},
			},
			destAddr: destAddr,
			wantOutputs: []btcutil.Amount{
				2842, 2763, 2645, 2467, 2400,
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
				},
				{
					outpoint: op2,
					value:    2_000,
				},
			},
			destAddr: destAddr,
			wantErr:  "for feeRate 568 sat/kw",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := presign(ctx, tc.presigner, tc.sweeps, tc.destAddr)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				outputs := tc.presigner.(*mockPresigner).outputs
				require.Equal(t, tc.wantOutputs, outputs)
			}
		})
	}
}

// TestCheckSignedTx tests that function CheckSignedTx checks all the criteria
// of CpfpHelper.SignTx correctly.
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
		tc := tc
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

// TestIsCPFPNeeded tests that function isCPFPNeeded works correctly, satisfying
// feeRateThresholdPPM.
func TestIsCPFPNeeded(t *testing.T) {
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

	witness := wire.TxWitness{
		make([]byte, 64),
	}

	cases := []struct {
		name           string
		parentTx       *wire.MsgTx
		inputAmt       btcutil.Amount
		numSweeps      int
		desiredFeeRate chainfee.SatPerKWeight
		signedFeeRate  chainfee.SatPerKWeight
		wantErr        string
		wantNeedsCPFP  bool
	}{
		{
			name: "fee rate matches exacly",
			parentTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness:          witness,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness:          witness,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
			},
			inputAmt:       3_000_000,
			numSweeps:      2,
			desiredFeeRate: 1000,
			wantErr:        "",
			wantNeedsCPFP:  false,
		},
		{
			name: "fee rate higher than needed",
			parentTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness:          witness,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness:          witness,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
			},
			inputAmt:       3_000_000,
			numSweeps:      2,
			desiredFeeRate: 900,
			wantErr:        "",
			wantNeedsCPFP:  false,
		},
		{
			name: "fee rate slightly lower than needed",
			parentTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness:          witness,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness:          witness,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
			},
			inputAmt:       3_000_000,
			numSweeps:      2,
			desiredFeeRate: 1020,
			wantErr:        "",
			wantNeedsCPFP:  false,
		},
		{
			name: "fee rate significantly lower than needed",
			parentTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness:          witness,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness:          witness,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
			},
			inputAmt:       3_000_000,
			numSweeps:      2,
			desiredFeeRate: 1100,
			wantErr:        "",
			wantNeedsCPFP:  true,
		},
		{
			name: "fewer inputs in parent transaction",
			parentTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness:          witness,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness:          witness,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
			},
			inputAmt:       3_000_000,
			numSweeps:      3,
			desiredFeeRate: 1100,
			wantErr:        "",
			wantNeedsCPFP:  false,
		},
		{
			name: "more inputs in parent transaction",
			parentTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness:          witness,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness:          witness,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
			},
			inputAmt:       3_000_000,
			numSweeps:      1,
			desiredFeeRate: 1100,
			wantErr:        "parent tx has more inputs",
		},
		{
			name: "signed fee rate equal to desired fee rate",
			parentTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness:          witness,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness:          witness,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2999374,
						PkScript: batchPkScript,
					},
				},
			},
			inputAmt:       3_000_000,
			numSweeps:      2,
			desiredFeeRate: 1100,
			signedFeeRate:  1100,
			wantErr:        "",
			wantNeedsCPFP:  false,
		},
		{
			name: "error: tx has negative fee",
			parentTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness:          witness,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness:          witness,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    3_001_000,
						PkScript: batchPkScript,
					},
				},
			},
			inputAmt:       3_000_000,
			numSweeps:      2,
			desiredFeeRate: 1000,
			wantErr:        "negative fee",
		},
		{
			name: "error: tx has multiple outputs",
			parentTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op1,
						Sequence:         1,
						Witness:          witness,
					},
					{
						PreviousOutPoint: op2,
						Sequence:         2,
						Witness:          witness,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    1_000_000,
						PkScript: batchPkScript,
					},
					{
						Value:    2_000_000,
						PkScript: batchPkScript,
					},
				},
			},
			inputAmt:       3_000_000,
			numSweeps:      2,
			desiredFeeRate: 1000,
			wantErr:        "must have one output",
		},
		{
			name: "error: unsigned tx",
			parentTx: &wire.MsgTx{
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
			},
			inputAmt:       3_000_000,
			numSweeps:      2,
			desiredFeeRate: 1000,
			wantErr:        "the tx must be signed",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			needsCPFP, err := isCPFPNeeded(
				tc.parentTx, tc.inputAmt, tc.numSweeps,
				tc.desiredFeeRate, tc.signedFeeRate,
			)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantNeedsCPFP, needsCPFP)
			}
		})
	}
}

// TestMakeUnsignedCPFP tests that function makeUnsignedCPFP works correctly,
// satisfying maxChildFeeSharePPM and making sure that child fee rate is higher
// than effective fee rate and of minRelayFee.
func TestMakeUnsignedCPFP(t *testing.T) {
	// Prepare the necessary data for test cases.
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

	batchTxid := chainhash.Hash{5, 5, 5}

	op := wire.OutPoint{
		Hash:  batchTxid,
		Index: 0,
	}

	cases := []struct {
		name              string
		parentTxid        chainhash.Hash
		parentOutput      btcutil.Amount
		parentWeight      lntypes.WeightUnit
		parentFee         btcutil.Amount
		minRelayFee       chainfee.SatPerKWeight
		effectiveFeeRate  chainfee.SatPerKWeight
		address           btcutil.Address
		currentHeight     int32
		wantErr           string
		wantUnsignedChild *wire.MsgTx
		wantChildFeeRate  chainfee.SatPerKWeight
	}{
		{
			name:             "normal child creation",
			parentTxid:       batchTxid,
			parentOutput:     2999374,
			parentWeight:     626,
			parentFee:        626,
			minRelayFee:      253,
			effectiveFeeRate: 2000,
			address:          p2trAddress,
			currentHeight:    800_000,
			wantUnsignedChild: &wire.MsgTx{
				Version:  2,
				LockTime: 800_000,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2997860,
						PkScript: p2trPkScript,
					},
				},
			},
			wantChildFeeRate: 3410,
		},
		{
			name:             "p2wpkh address",
			parentTxid:       batchTxid,
			parentOutput:     2999374,
			parentWeight:     626,
			parentFee:        626,
			minRelayFee:      253,
			effectiveFeeRate: 2000,
			address:          destAddr,
			currentHeight:    800_000,
			wantUnsignedChild: &wire.MsgTx{
				Version:  2,
				LockTime: 800_000,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2997870,
						PkScript: batchPkScript,
					},
				},
			},
			wantChildFeeRate: 3426,
		},
		{
			name:             "error: p2pk address",
			parentTxid:       batchTxid,
			parentOutput:     2999374,
			parentWeight:     626,
			parentFee:        626,
			minRelayFee:      253,
			effectiveFeeRate: 2000,
			address:          p2pkAddress,
			currentHeight:    800_000,
			wantErr:          "unknown address type",
		},
		{
			name:             "effective feerate as in parent",
			parentTxid:       batchTxid,
			parentOutput:     2999374,
			parentWeight:     626,
			parentFee:        626,
			minRelayFee:      253,
			effectiveFeeRate: 1000,
			address:          p2trAddress,
			currentHeight:    800_000,
			wantUnsignedChild: &wire.MsgTx{
				Version:  2,
				LockTime: 800_000,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2998930,
						PkScript: p2trPkScript,
					},
				},
			},
			wantChildFeeRate: 1000,
		},
		{
			name:             "effective feerate below parent",
			parentTxid:       batchTxid,
			parentOutput:     2999374,
			parentWeight:     626,
			parentFee:        626,
			minRelayFee:      253,
			effectiveFeeRate: 500,
			address:          p2trAddress,
			currentHeight:    800_000,
			wantErr:          "lower than effective fee rate",
		},
		{
			name:             "high minRelayFee",
			parentTxid:       batchTxid,
			parentOutput:     2999374,
			parentWeight:     626,
			parentFee:        626,
			minRelayFee:      10_000,
			effectiveFeeRate: 2000,
			address:          p2trAddress,
			currentHeight:    800_000,
			wantUnsignedChild: &wire.MsgTx{
				Version:  2,
				LockTime: 800_000,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: op,
					},
				},
				TxOut: []*wire.TxOut{
					{
						Value:    2994934,
						PkScript: p2trPkScript,
					},
				},
			},
			wantChildFeeRate: 10_000,
		},
		{
			name:             "child fee too high",
			parentTxid:       batchTxid,
			parentOutput:     2999374,
			parentWeight:     626,
			parentFee:        626,
			minRelayFee:      253,
			effectiveFeeRate: 750_000,
			address:          p2trAddress,
			currentHeight:    800_000,
			wantErr:          "is higher than 20% of total funds",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			childTx, childFeeRate, err := makeUnsignedCPFP(
				tc.parentTxid, tc.parentOutput, tc.parentWeight,
				tc.parentFee, tc.minRelayFee,
				tc.effectiveFeeRate, tc.address,
				tc.currentHeight,
			)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantUnsignedChild, childTx)
				require.Equal(
					t, tc.wantChildFeeRate, childFeeRate,
				)
			}
		})
	}
}
