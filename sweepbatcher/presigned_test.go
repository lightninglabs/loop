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

// TestOrderedSweeps checks that methods batch.getOrderedSweeps and
// batch.getSweepsGroups works properly.
func TestOrderedSweeps(t *testing.T) {
	// Prepare the necessary data for test cases.
	op1 := wire.OutPoint{Hash: chainhash.Hash{1, 1, 1}, Index: 1}
	op2 := wire.OutPoint{Hash: chainhash.Hash{2, 2, 2}, Index: 2}
	op3 := wire.OutPoint{Hash: chainhash.Hash{3, 3, 3}, Index: 3}
	op4 := wire.OutPoint{Hash: chainhash.Hash{4, 4, 4}, Index: 4}
	op5 := wire.OutPoint{Hash: chainhash.Hash{5, 5, 5}, Index: 5}
	op6 := wire.OutPoint{Hash: chainhash.Hash{6, 6, 6}, Index: 6}

	swapHash1 := lntypes.Hash{1, 1}
	swapHash2 := lntypes.Hash{2, 2}
	swapHash3 := lntypes.Hash{3, 3}

	ctx := context.Background()

	cases := []struct {
		name        string
		sweeps      []sweep
		skippedTxns map[chainhash.Hash]struct{}

		// Testing errors.
		skipStore     bool
		reverseStore  bool
		replaceSweeps map[wire.OutPoint]sweep

		wantGroups [][]sweep
		wantErr1   string
		wantErr2   string
	}{
		{
			name:       "no sweeps",
			sweeps:     []sweep{},
			wantGroups: [][]sweep{},
		},

		{
			name: "one sweep",
			sweeps: []sweep{
				{
					outpoint: op1,
					swapHash: swapHash1,
				},
			},
			wantGroups: [][]sweep{
				{
					{
						outpoint: op1,
						swapHash: swapHash1,
					},
				},
			},
		},

		{
			name: "one sweep, skipped",
			sweeps: []sweep{
				{
					outpoint: op1,
					swapHash: swapHash1,
				},
			},
			skippedTxns: map[chainhash.Hash]struct{}{
				op1.Hash: {},
			},
			wantGroups: [][]sweep{},
		},

		{
			name: "two sweeps, one swap",
			sweeps: []sweep{
				{
					outpoint: op2,
					swapHash: swapHash1,
				},
				{
					outpoint: op1,
					swapHash: swapHash1,
				},
			},
			wantGroups: [][]sweep{
				{
					{
						outpoint: op2,
						swapHash: swapHash1,
					},
					{
						outpoint: op1,
						swapHash: swapHash1,
					},
				},
			},
		},

		{
			name: "two sweeps, one swap, one skipped",
			sweeps: []sweep{
				{
					outpoint: op2,
					swapHash: swapHash1,
				},
				{
					outpoint: op1,
					swapHash: swapHash1,
				},
			},
			skippedTxns: map[chainhash.Hash]struct{}{
				op1.Hash: {},
			},
			wantGroups: [][]sweep{
				{
					{
						outpoint: op2,
						swapHash: swapHash1,
					},
				},
			},
		},

		{
			name: "two sweeps, two swap",
			sweeps: []sweep{
				{
					outpoint: op2,
					swapHash: swapHash1,
				},
				{
					outpoint: op1,
					swapHash: swapHash2,
				},
			},
			wantGroups: [][]sweep{
				{
					{
						outpoint: op2,
						swapHash: swapHash1,
					},
				},
				{
					{
						outpoint: op1,
						swapHash: swapHash2,
					},
				},
			},
		},

		{
			name: "many sweeps and swaps",
			sweeps: []sweep{
				{
					outpoint: op1,
					swapHash: swapHash1,
				},
				{
					outpoint: op2,
					swapHash: swapHash1,
				},
				{
					outpoint: op3,
					swapHash: swapHash1,
				},
				{
					outpoint: op4,
					swapHash: swapHash2,
				},
				{
					outpoint: op5,
					swapHash: swapHash3,
				},
				{
					outpoint: op6,
					swapHash: swapHash3,
				},
			},
			wantGroups: [][]sweep{
				{
					{
						outpoint: op1,
						swapHash: swapHash1,
					},
					{
						outpoint: op2,
						swapHash: swapHash1,
					},
					{
						outpoint: op3,
						swapHash: swapHash1,
					},
				},
				{
					{
						outpoint: op4,
						swapHash: swapHash2,
					},
				},
				{
					{
						outpoint: op5,
						swapHash: swapHash3,
					},
					{
						outpoint: op6,
						swapHash: swapHash3,
					},
				},
			},
		},

		{
			name: "error: sweeps not stored in DB",
			sweeps: []sweep{
				{
					outpoint: op1,
					swapHash: swapHash1,
				},
			},
			skipStore: true,
			wantErr1:  "returned 0 sweeps, len(b.sweeps) is 1",
		},

		{
			name: "error: wrong order in DB",
			sweeps: []sweep{
				{
					outpoint: op1,
					swapHash: swapHash1,
				},
				{
					outpoint: op2,
					swapHash: swapHash2,
				},
			},
			reverseStore: true,
		},

		{
			name: "error: extra sweep in DB",
			sweeps: []sweep{
				{
					outpoint: op1,
					swapHash: swapHash1,
				},
				{
					outpoint: op2,
					swapHash: swapHash2,
				},
			},
			replaceSweeps: map[wire.OutPoint]sweep{
				op2: {
					outpoint: op3,
					swapHash: swapHash3,
				},
			},
			wantErr1: "returned unknown sweep",
		},

		{
			name: "error: swaps interleaved",
			sweeps: []sweep{
				{
					outpoint: op1,
					swapHash: swapHash1,
				},
				{
					outpoint: op2,
					swapHash: swapHash2,
				},
				{
					outpoint: op3,
					swapHash: swapHash1,
				},
			},
			wantErr2: "3 groups of sweeps and 2 distinct swaps",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Create a map of sweeps.
			m := make(map[wire.OutPoint]sweep, len(tc.sweeps))
			for _, s := range tc.sweeps {
				require.NotContains(t, m, s.outpoint)
				m[s.outpoint] = s
			}

			// Create a batch.
			b := &batch{
				sweeps: m,
				store:  NewStoreMock(),
				cfg: &batchConfig{
					skippedTxns: tc.skippedTxns,
				},
			}

			// Store the sweeps in mock store.
			switch {
			// Don't create DB sweeps at all.
			case tc.skipStore:

			// Reverse the order of DB sweeps to ID incorrect IDs.
			case tc.reverseStore:
				for i := len(tc.sweeps) - 1; i >= 0; i-- {
					s := tc.sweeps[i]
					const completed = false
					err := b.persistSweep(ctx, s, completed)
					require.NoError(t, err)
				}

			// Working DB sweeps.
			default:
				for _, s := range tc.sweeps {
					const completed = false
					err := b.persistSweep(ctx, s, completed)
					require.NoError(t, err)
				}
			}

			// Replace some sweeps to test an error.
			for removed, added := range tc.replaceSweeps {
				require.Contains(t, m, removed)
				delete(m, removed)
				require.NotContains(t, m, added.outpoint)
				m[added.outpoint] = added
			}

			// Remove skipped sweeps from the batch to make it
			// match with what is read from DB after filtering.
			for op := range m {
				if _, has := tc.skippedTxns[op.Hash]; has {
					delete(m, op)
				}
			}

			// Now run the tested functions.
			orderedSweeps, err := b.getOrderedSweeps(ctx)
			if tc.wantErr1 != "" {
				require.ErrorContains(t, err, tc.wantErr1)
				return
			}
			require.NoError(t, err)

			if tc.reverseStore {
				require.NotEqual(t, tc.sweeps, orderedSweeps)
				return
			}

			// The wanted list of sweeps matches the input order.
			notSkipped := make([]sweep, 0, len(tc.sweeps))
			for _, s := range tc.sweeps {
				_, has := tc.skippedTxns[s.outpoint.Hash]
				if has {
					continue
				}
				notSkipped = append(notSkipped, s)
			}
			require.Equal(t, notSkipped, orderedSweeps)

			groups, err := b.getSweepsGroups(ctx)
			if tc.wantErr2 != "" {
				require.ErrorContains(t, err, tc.wantErr2)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantGroups, groups)
		})
	}
}

// mockPresignedTxChecker is an implementation of presignedTxChecker used in
// TestEnsurePresigned.
type mockPresignedTxChecker struct {
	// primarySweepID is the value the mock matches this argument against.
	primarySweepID wire.OutPoint

	// destPkScript is the value returned by mock.DestPkScript.
	destPkScript []byte

	// signTxCalls is the number of SignTx calls.
	signTxCalls int

	// recordedInputAmt is the saved value of inputAmt argument of SignTx.
	recordedInputAmt btcutil.Amount

	// recordedMinRelayFee is the saved value of minRelayFee argument of
	// SignTx.
	recordedMinRelayFee chainfee.SatPerKWeight

	// recordedFeeRate is the saved value of feeRate argument of SignTx.
	recordedFeeRate chainfee.SatPerKWeight

	// recordedLoadOnly is the saved value of loadOnly argument of SignTx.
	recordedLoadOnly bool

	// destPkScriptErr is the error returned by DestPkScript (if any).
	destPkScriptErr error

	// signedTxErr is the error returned by SignTx (if any).
	signedTxErr error
}

// DestPkScript returns destination pkScript used by the sweep batch.
func (m *mockPresignedTxChecker) DestPkScript(ctx context.Context,
	primarySweepID wire.OutPoint) ([]byte, error) {

	if primarySweepID != m.primarySweepID {
		return nil, fmt.Errorf("primarySweepID mismatch")
	}

	if m.destPkScriptErr != nil {
		return nil, m.destPkScriptErr
	}

	return m.destPkScript, nil
}

// SignTx records all its argumentd and returned a "presigned" tx.
func (m *mockPresignedTxChecker) SignTx(ctx context.Context,
	primarySweepID wire.OutPoint, tx *wire.MsgTx, inputAmt btcutil.Amount,
	minRelayFee, feeRate chainfee.SatPerKWeight,
	loadOnly bool) (*wire.MsgTx, error) {

	m.signTxCalls++

	if primarySweepID != m.primarySweepID {
		return nil, fmt.Errorf("primarySweepID mismatch")
	}

	m.recordedInputAmt = inputAmt
	m.recordedMinRelayFee = minRelayFee
	m.recordedFeeRate = feeRate
	m.recordedLoadOnly = loadOnly

	if m.signedTxErr != nil {
		return nil, m.signedTxErr
	}

	// Pretend that we have a presigned transaction.
	tx = tx.Copy()
	for i := range tx.TxIn {
		tx.TxIn[i].Witness = wire.TxWitness{
			make([]byte, 64),
		}
	}

	return tx, nil
}

// TestEnsurePresigned checks that function ensurePresigned works correctly.
func TestEnsurePresigned(t *testing.T) {
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

	ctx := context.Background()

	cases := []struct {
		name            string
		primarySweepID  wire.OutPoint
		sweeps          []*sweep
		destPkScript    []byte
		wantInputAmt    btcutil.Amount
		destPkScriptErr error
		signedTxErr     error
	}{
		{
			name:           "one input",
			primarySweepID: op1,
			sweeps: []*sweep{
				{
					outpoint: op1,
					value:    1_000_000,
					timeout:  1000,
				},
			},
			destPkScript: batchPkScript,
			wantInputAmt: 1_000_000,
		},

		{
			name:           "two inputs",
			primarySweepID: op1,
			sweeps: []*sweep{
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
			destPkScript: batchPkScript,
			wantInputAmt: 3_000_000,
		},

		{
			name:           "error: DestPkScript fails",
			primarySweepID: op1,
			sweeps: []*sweep{
				{
					outpoint: op1,
					value:    1_000_000,
					timeout:  1000,
				},
			},
			destPkScriptErr: fmt.Errorf("test DestPkScript error"),
		},

		{
			name:           "error: SignTx fails",
			primarySweepID: op1,
			sweeps: []*sweep{
				{
					outpoint: op1,
					value:    1_000_000,
					timeout:  1000,
				},
			},
			destPkScript: batchPkScript,
			signedTxErr:  fmt.Errorf("test SignTx error"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &mockPresignedTxChecker{
				primarySweepID:  tc.primarySweepID,
				destPkScript:    tc.destPkScript,
				destPkScriptErr: tc.destPkScriptErr,
				signedTxErr:     tc.signedTxErr,
			}

			err := ensurePresigned(
				ctx, tc.sweeps, c,
				&chaincfg.RegressionNetParams,
			)
			switch {
			case tc.destPkScriptErr != nil:
				require.ErrorIs(t, err, tc.destPkScriptErr)
			case tc.signedTxErr != nil:
				require.ErrorIs(t, err, tc.signedTxErr)
			default:
				require.NoError(t, err)
				require.Equal(t, 1, c.signTxCalls)
				require.Equal(
					t, tc.wantInputAmt, c.recordedInputAmt,
				)
				require.Equal(
					t, chainfee.FeePerKwFloor,
					c.recordedMinRelayFee,
				)
				require.Equal(
					t, chainfee.FeePerKwFloor,
					c.recordedFeeRate,
				)
				require.True(t, c.recordedLoadOnly)
			}
		})
	}
}

// hasInput returns if the transaction spends the UTXO.
func hasInput(tx *wire.MsgTx, utxo wire.OutPoint) bool {
	for _, txIn := range tx.TxIn {
		if txIn.PreviousOutPoint == utxo {
			return true
		}
	}

	return false
}

// mockPresigner is an implementation of Presigner used in TestPresign.
type mockPresigner struct {
	// outputs collects outputs of presigned transactions.
	outputs []btcutil.Amount

	// lockTimes collects LockTime's of presigned transactions.
	lockTimes []uint32

	// failAt is optional index of a call at which it fails, 1 based.
	failAt int
}

// SignTx memorizes the value of the output and fails if the number of
// calls previously made is failAt.
func (p *mockPresigner) SignTx(ctx context.Context,
	primarySweepID wire.OutPoint, tx *wire.MsgTx, inputAmt btcutil.Amount,
	minRelayFee, feeRate chainfee.SatPerKWeight,
	loadOnly bool) (*wire.MsgTx, error) {

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if !hasInput(tx, primarySweepID) {
		return nil, fmt.Errorf("primarySweepID %v not in tx",
			primarySweepID)
	}

	if len(p.outputs)+1 == p.failAt {
		return nil, fmt.Errorf("test error in SignTx")
	}

	p.outputs = append(p.outputs, btcutil.Amount(tx.TxOut[0].Value))
	p.lockTimes = append(p.lockTimes, tx.LockTime)

	return tx, nil
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
		primarySweepID   wire.OutPoint
		sweeps           []sweep
		destAddr         btcutil.Address
		nextBlockFeeRate chainfee.SatPerKWeight
		wantErr          string
		wantOutputs      []btcutil.Amount
		wantLockTimes    []uint32
	}{
		{
			name:           "error: no presigner",
			primarySweepID: op1,
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
			primarySweepID:   op1,
			presigner:        &mockPresigner{},
			destAddr:         destAddr,
			nextBlockFeeRate: chainfee.FeePerKwFloor,
			wantErr:          "there are no sweeps",
		},

		{
			name:           "error: no destAddr",
			presigner:      &mockPresigner{},
			primarySweepID: op1,
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
			name:           "error: zero nextBlockFeeRate",
			presigner:      &mockPresigner{},
			primarySweepID: op1,
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
			name:           "error: timeout is not set",
			presigner:      &mockPresigner{},
			primarySweepID: op1,
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
			name:      "error: primary not set",
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
			wantErr:          "not in tx",
		},

		{
			name:           "error: primary not in tx",
			presigner:      &mockPresigner{},
			primarySweepID: op2,
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
					timeout:  1000,
				},
			},
			destAddr:         destAddr,
			nextBlockFeeRate: chainfee.FeePerKwFloor,
			wantErr:          "not in tx",
		},

		{
			name:           "one sweep",
			presigner:      &mockPresigner{},
			primarySweepID: op1,
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
					timeout:  1000,
				},
			},
			destAddr:         destAddr,
			nextBlockFeeRate: chainfee.FeePerKwFloor,
			wantOutputs: []btcutil.Amount{
				999900, 999880, 999856, 999827, 999793, 999752,
				999702, 999643, 999572, 999486, 999384, 999260,
				999113, 998935, 998723, 998467, 998161, 997793,
				997352, 996823, 996187, 995425, 994510, 993413,
				992096, 990515, 988618, 986342, 983610, 980332,
				976399, 971679, 966015, 959218, 951062, 941274,
				929530, 915435, 898523, 878227, 853873, 824648,
			},
			wantLockTimes: []uint32{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 950, 950, 950,
				950, 950, 950, 950, 950, 950, 950, 950, 950,
				950, 950, 950, 950,
			},
		},

		{
			name:           "two sweeps",
			presigner:      &mockPresigner{},
			primarySweepID: op1,
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
				2999841, 2999810, 2999773, 2999728, 2999673,
				2999608, 2999530, 2999436, 2999323, 2999188,
				2999026, 2998831, 2998598, 2998317, 2997981,
				2997577, 2997093, 2996512, 2995814, 2994977,
				2993973, 2992768, 2991322, 2989587, 2987505,
				2985006, 2982007, 2978409, 2974091, 2968910,
				2962691, 2955230, 2946276, 2935532, 2922639,
				2907167, 2888600, 2866320, 2839584, 2807501,
				2769001, 2722802,
			},
			wantLockTimes: []uint32{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 950, 950, 950,
				950, 950, 950, 950, 950, 950, 950, 950, 950,
				950, 950, 950, 950,
			},
		},

		{
			name:           "two sweeps, another primary",
			presigner:      &mockPresigner{},
			primarySweepID: op2,
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
				2999841, 2999810, 2999773, 2999728, 2999673,
				2999608, 2999530, 2999436, 2999323, 2999188,
				2999026, 2998831, 2998598, 2998317, 2997981,
				2997577, 2997093, 2996512, 2995814, 2994977,
				2993973, 2992768, 2991322, 2989587, 2987505,
				2985006, 2982007, 2978409, 2974091, 2968910,
				2962691, 2955230, 2946276, 2935532, 2922639,
				2907167, 2888600, 2866320, 2839584, 2807501,
				2769001, 2722802,
			},
			wantLockTimes: []uint32{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 950, 950, 950,
				950, 950, 950, 950, 950, 950, 950, 950, 950,
				950, 950, 950, 950,
			},
		},

		{
			name:           "timeout < 50",
			presigner:      &mockPresigner{},
			primarySweepID: op1,
			sweeps: []sweep{
				{
					outpoint: op1,
					value:    1_000_000,
					timeout:  40,
				},
				{
					outpoint: op2,
					value:    2_000_000,
					timeout:  40,
				},
			},
			destAddr:         destAddr,
			nextBlockFeeRate: 50 * chainfee.FeePerKwFloor,
			wantOutputs: []btcutil.Amount{
				2999841, 2999810, 2999773, 2999728, 2999673,
				2999608, 2999530, 2999436, 2999323, 2999188,
				2999026, 2998831, 2998598, 2998317, 2997981,
				2997577, 2997093, 2996512, 2995814, 2994977,
				2993973, 2992768, 2991322, 2989587, 2987505,
				2985006, 2982007, 2978409, 2974091, 2968910,
				2962691, 2955230, 2946276, 2935532, 2922639,
				2907167, 2888600, 2866320, 2839584, 2807501,
				2769001, 2722802,
			},
			wantLockTimes: []uint32{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
			},
		},

		{
			name:           "high current feerate => locktime later",
			presigner:      &mockPresigner{},
			primarySweepID: op1,
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
				2999841, 2999810, 2999773, 2999728, 2999673,
				2999608, 2999530, 2999436, 2999323, 2999188,
				2999026, 2998831, 2998598, 2998317, 2997981,
				2997577, 2997093, 2996512, 2995814, 2994977,
				2993973, 2992768, 2991322, 2989587, 2987505,
				2985006, 2982007, 2978409, 2974091, 2968910,
				2962691, 2955230, 2946276, 2935532, 2922639,
				2907167, 2888600, 2866320, 2839584, 2807501,
				2769001, 2722802,
			},
			wantLockTimes: []uint32{
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 950, 950, 950, 950, 950, 950, 950,
			},
		},

		{
			name:           "small amount => fewer steps until clamped",
			presigner:      &mockPresigner{},
			primarySweepID: op1,
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
				2841, 2810, 2773, 2728, 2673, 2608, 2530, 2436,
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
			primarySweepID: op1,
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
				ctx, tc.presigner, tc.destAddr,
				tc.primarySweepID, tc.sweeps,
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
			name: "unsigned input in signedTx",
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
			wantErr:     "is not signed",
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
