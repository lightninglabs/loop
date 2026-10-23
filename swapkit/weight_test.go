package swapkit

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestAssetWitnessWeights compares both estimators with complete witnesses and
// transaction serializations for every supported asset anchor contract.
func TestAssetWitnessWeights(t *testing.T) {
	root := swapKitTestAssetRoot()
	params := swapKitTestParams(t)
	type spendCase struct {
		name    string
		spend   *SwapAnchorSpendInfo
		success bool
		witness func([]byte) ([][]byte, error)
	}
	var cases []spendCase
	for _, policy := range []SwapHTLCPolicy{
		SwapHTLCLegacyCSV, SwapHTLCCLTV,
	} {
		params.Policy = policy
		kit, err := NewSwapKit(params)
		require.NoError(t, err)
		for _, success := range []bool{false, true} {
			spend, _ := swapKitTestSpend(t, kit, root, success)
			require.Len(t, spend.ControlBlock, 97)
			witness := func(signature []byte) ([][]byte, error) {
				return kit.TimeoutWitness(root, signature)
			}
			if success {
				witness = func(signature []byte) ([][]byte, error) {
					return kit.SuccessWitness(root, signature, [32]byte{42})
				}
			}
			cases = append(cases, spendCase{
				name:  swapKitTestName(policy, success, ""),
				spend: spend, success: success, witness: witness,
			})
		}
	}
	deposit, err := NewDepositKit(DepositKitParams{
		ClientKey: params.SenderKey, ServerKey: params.ReceiverKey,
		CSVExpiry: 144,
	})
	require.NoError(t, err)
	depositSpend, err := deposit.TimeoutSpend(root)
	require.NoError(t, err)
	require.Len(t, depositSpend.ControlBlock, 65)
	cases = append(cases, spendCase{
		name: "deposit CSV refund", spend: depositSpend,
		witness: func(signature []byte) ([][]byte, error) {
			return deposit.TimeoutWitness(root, signature)
		},
	})
	reservation, err := NewReservationKit(ReservationKitParams{
		ServerKey: params.SenderKey, ClientKey: params.ReceiverKey,
		RefundHeight: 1_000_000,
	})
	require.NoError(t, err)
	reservationSpend, err := reservation.TimeoutSpend(root)
	require.NoError(t, err)
	require.Len(t, reservationSpend.ControlBlock, 65)
	cases = append(cases, spendCase{
		name: "reservation CLTV refund", spend: reservationSpend,
		witness: func(signature []byte) ([][]byte, error) {
			return reservation.TimeoutWitness(root, signature)
		},
	})

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			key := byte(1)
			if test.success {
				key = 2
			}
			tx, _, signature := swapKitSignTestTx(t, test.spend, key, nil)
			for _, explicitSighash := range []bool{false, true} {
				signature := bytes.Clone(signature)
				if explicitSighash {
					signature = append(signature, byte(txscript.SigHashAll))
				}
				tx.TxIn[0].Witness, err = test.witness(signature)
				require.NoError(t, err)
				var full, stripped bytes.Buffer
				require.NoError(t, tx.Serialize(&full))
				require.NoError(t, tx.SerializeNoWitness(&stripped))
				witnessSize := tx.TxIn[0].Witness.SerializeSize()
				require.Equal(t, witnessSize, full.Len()-stripped.Len()-2)

				var estimator input.TxWeightEstimator
				maxSize := test.spend.MaxTimeoutWitnessSize()
				if test.success {
					maxSize = test.spend.MaxSuccessWitnessSize()
					test.spend.AddSuccessToEstimator(&estimator)
				} else {
					test.spend.AddTimeoutToEstimator(&estimator)
				}
				estimator.AddTxOutput(tx.TxOut[0])
				padding := schnorr.SignatureSize + 1 - len(signature)
				require.EqualValues(t, witnessSize+padding, maxSize)
				weight := 3*stripped.Len() + full.Len() + padding
				require.Equal(t, lntypes.WeightUnit(weight), estimator.Weight())
			}
		})
	}
}

// TestCLTVRefundRBFDefaults checks new templates signal replacement while the
// old non-RBF sequence still satisfies the same immutable funded contract.
func TestCLTVRefundRBFDefaults(t *testing.T) {
	root := swapKitTestAssetRoot()
	params := swapKitTestParams(t)
	params.Policy = SwapHTLCCLTV
	htlc, err := NewSwapKit(params)
	require.NoError(t, err)
	htlcSpend, err := htlc.TimeoutSpend(root)
	require.NoError(t, err)
	reservation, err := NewReservationKit(ReservationKitParams{
		ServerKey: params.SenderKey, ClientKey: params.ReceiverKey,
		RefundHeight: params.Expiry,
	})
	require.NoError(t, err)
	reservationSpend, err := reservation.TimeoutSpend(root)
	require.NoError(t, err)
	for name, spend := range map[string]*SwapAnchorSpendInfo{
		"HTLC": htlcSpend, "reservation": reservationSpend,
	} {
		t.Run(name, func(t *testing.T) {
			require.EqualValues(t, 0xfffffffd, spend.Sequence)
			require.Less(t, spend.Sequence, wire.MaxTxInSequenceNum-1)
			require.NotZero(t, spend.Sequence&wire.SequenceLockTimeDisabled)
			for _, sequence := range []uint32{
				spend.Sequence, wire.MaxTxInSequenceNum - 1,
			} {
				tx, previous, signature := swapKitSignTestTx(
					t, spend, 1, func(tx *wire.MsgTx) {
						tx.TxIn[0].Sequence = sequence
					},
				)
				tx.TxIn[0].Witness, err = spend.witness(signature, nil)
				require.NoError(t, err)
				require.NoError(t, swapKitExecuteTestTx(tx, previous))
			}
		})
	}
}
