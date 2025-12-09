package withdraw

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestNewManagerHeightValidation ensures the constructor rejects zero heights.
func TestNewManagerHeightValidation(t *testing.T) {
	t.Parallel()

	cfg := &ManagerConfig{}

	_, err := NewManager(cfg, 0)
	require.ErrorContains(t, err, "invalid current height 0")

	manager, err := NewManager(cfg, 1)
	require.NoError(t, err)
	require.NotNil(t, manager)
}

// TestSignMusig2Tx_MissingSigningInfo tests that signMusig2Tx should error
// when sigInfo is missing an entry for one of the deposits.
//
// This test documents expected behavior. The function should validate that
// len(sigInfo) == len(sessions) and all sessions have corresponding sigInfo
// entries before attempting to sign, returning an error if validation fails.
func TestSignMusig2Tx_MissingSigningInfo(t *testing.T) {
	t.Parallel()

	// Create a dummy transaction with two inputs.
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  [32]byte{1},
			Index: 0,
		},
	})
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  [32]byte{2},
			Index: 0,
		},
	})

	// Add a dummy output with a simple pkScript.
	pkScript := []byte{
		0x51, 0x20, // OP_1 OP_PUSHBYTES_32
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	tx.AddTxOut(&wire.TxOut{
		Value:    10000,
		PkScript: pkScript,
	})

	// Create deposit keys for both inputs.
	deposit1Key := "0100000000000000000000000000000000000000000000000000000000000000:0"
	deposit2Key := "0200000000000000000000000000000000000000000000000000000000000000:0"

	// Create sessions for both deposits.
	sessions := map[string]*input.MuSig2SessionInfo{
		deposit1Key: {
			SessionID: [32]byte{1},
		},
		deposit2Key: {
			SessionID: [32]byte{2},
		},
	}

	// Create sigInfo with only one entry (missing deposit2).
	sigInfo := map[string]*swapserverrpc.ServerPsbtWithdrawSigningInfo{
		deposit1Key: {
			Nonce: make([]byte, 66),
			Sig:   make([]byte, 64),
		},
	}

	// Create depositsToIdx mapping both deposits.
	depositsToIdx := map[string]int{
		deposit1Key: 0,
		deposit2Key: 1,
	}

	// Create prevOutFetcher.
	prevOuts := map[wire.OutPoint]*wire.TxOut{
		tx.TxIn[0].PreviousOutPoint: {
			Value:    5000,
			PkScript: pkScript,
		},
		tx.TxIn[1].PreviousOutPoint: {
			Value:    5000,
			PkScript: pkScript,
		},
	}
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)

	// Create a mock signer.
	lnd := test.NewMockLnd()
	signer := lnd.Signer

	// Create a minimal manager.
	m := &Manager{
		cfg: &ManagerConfig{
			Signer: signer,
		},
	}

	// Call signMusig2Tx - it should error because sigInfo is missing
	// an entry for deposit2.
	//
	// The function should validate that:
	// 1. len(sigInfo) == len(sessions) == len(tx.TxIn)
	// 2. All keys in sessions exist in sigInfo
	// 3. No partial signing is allowed
	//
	// This test verifies that the function errors when sigInfo is
	// incomplete, preventing a partially signed transaction.
	ctx := context.Background()
	_, err := m.signMusig2Tx(
		ctx, prevOutFetcher, signer, tx, sessions, sigInfo,
		depositsToIdx,
	)

	// Expect an error. The function should validate that sigInfo has
	// entries for all sessions before attempting to sign.
	require.ErrorContains(t, err, "unexpected number of partial "+
		"signatures from server")
}

// TestSignMusig2Tx_MismatchedIndex tests that signMusig2Tx should error when
// sigInfo has all expected keys but one maps to the wrong index in depositsToIdx.
//
// This test documents expected behavior. The function should validate that
// each deposit maps to a unique index in [0, len(tx.TxIn)-1] before signing,
// returning an error if indices conflict or are out of bounds.
func TestSignMusig2Tx_MismatchedIndex(t *testing.T) {
	t.Parallel()

	// Create a dummy transaction with two inputs.
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  [32]byte{1},
			Index: 0,
		},
	})
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  [32]byte{2},
			Index: 0,
		},
	})

	// Add a dummy output with a simple pkScript.
	pkScript := []byte{
		0x51, 0x20, // OP_1 OP_PUSHBYTES_32
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	tx.AddTxOut(&wire.TxOut{
		Value:    10000,
		PkScript: pkScript,
	})

	// Create deposit keys for both inputs.
	deposit1Key := "0000000000000000000000000000000000000000000000000000000000000001:0"
	deposit2Key := "0000000000000000000000000000000000000000000000000000000000000002:0"

	// Create sessions for both deposits.
	sessions := map[string]*input.MuSig2SessionInfo{
		deposit1Key: {
			SessionID: [32]byte{1},
		},
		deposit2Key: {
			SessionID: [32]byte{2},
		},
	}

	// Create sigInfo with both entries.
	sigInfo := map[string]*swapserverrpc.ServerPsbtWithdrawSigningInfo{
		deposit1Key: {
			Nonce: make([]byte, 66),
			Sig:   make([]byte, 64),
		},
		deposit2Key: {
			Nonce: make([]byte, 66),
			Sig:   make([]byte, 64),
		},
	}

	// Create depositsToIdx with WRONG mapping for deposit2.
	// deposit2 should map to index 1, but we map it to 0.
	depositsToIdx := map[string]int{
		deposit1Key: 0,
		deposit2Key: 0, // Wrong! Should be 1.
	}

	// Create prevOutFetcher.
	prevOuts := map[wire.OutPoint]*wire.TxOut{
		tx.TxIn[0].PreviousOutPoint: {
			Value:    5000,
			PkScript: pkScript,
		},
		tx.TxIn[1].PreviousOutPoint: {
			Value:    5000,
			PkScript: pkScript,
		},
	}
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)

	// Create a mock signer.
	lnd := test.NewMockLnd()
	signer := lnd.Signer

	// Create a minimal manager.
	m := &Manager{
		cfg: &ManagerConfig{
			Signer: signer,
		},
	}

	// Call signMusig2Tx - it should error because depositsToIdx has
	// a mismatched index for deposit2.
	//
	// The function should validate that:
	// 1. Each deposit in depositsToIdx maps to a unique index
	// 2. All indices from 0 to len(tx.TxIn)-1 are covered exactly once
	// 3. No index is used twice (which would cause signature overwrites)
	//
	// In this case, both deposit1 and deposit2 map to index 0, which
	// is invalid. The second deposit would overwrite the witness of
	// the first input, resulting in an invalid transaction.
	ctx := context.Background()
	_, err := m.signMusig2Tx(
		ctx, prevOutFetcher, signer, tx, sessions, sigInfo,
		depositsToIdx,
	)

	// Expect an error. The function should validate index uniqueness.
	require.ErrorContains(t, err, "deposit index maps wrong tx index")
}

// TestSignMusig2Tx_MissingOutpointInDepositMap tests that signMusig2Tx errors
// when a transaction input's outpoint is not present in depositsToIdx map.
//
// This test validates that the function checks all transaction inputs have
// corresponding entries in the depositsToIdx map before signing.
func TestSignMusig2Tx_MissingOutpointInDepositMap(t *testing.T) {
	t.Parallel()

	// Create a dummy transaction with two inputs.
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  [32]byte{1},
			Index: 0,
		},
	})
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  [32]byte{2},
			Index: 0,
		},
	})

	// Add a dummy output with a simple pkScript.
	pkScript := []byte{
		0x51, 0x20, // OP_1 OP_PUSHBYTES_32
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	tx.AddTxOut(&wire.TxOut{
		Value:    10000,
		PkScript: pkScript,
	})

	// Create deposit keys for both inputs.
	deposit1Key := "0100000000000000000000000000000000000000000000000000000000000000:0"
	deposit2Key := "0200000000000000000000000000000000000000000000000000000000000000:0"

	// Create sessions for both deposits.
	sessions := map[string]*input.MuSig2SessionInfo{
		deposit1Key: {
			SessionID: [32]byte{1},
		},
		deposit2Key: {
			SessionID: [32]byte{2},
		},
	}

	// Create sigInfo with both entries.
	sigInfo := map[string]*swapserverrpc.ServerPsbtWithdrawSigningInfo{
		deposit1Key: {
			Nonce: make([]byte, 66),
			Sig:   make([]byte, 64),
		},
		deposit2Key: {
			Nonce: make([]byte, 66),
			Sig:   make([]byte, 64),
		},
	}

	// Create a third deposit key that doesn't correspond to any tx input.
	deposit3Key := "0300000000000000000000000000000000000000000000000000000000000000:0"

	// Create depositsToIdx with deposit1 at correct index, but deposit3
	// (which doesn't exist as a tx input) instead of deposit2.
	// This means when the function iterates through tx.TxIn, it will find
	// deposit1 in the map, but deposit2 (the actual second input) won't
	// be found.
	depositsToIdx := map[string]int{
		deposit1Key: 0,
		deposit3Key: 1, // Wrong key - this isn't a real tx input
	}

	// Create prevOutFetcher.
	prevOuts := map[wire.OutPoint]*wire.TxOut{
		tx.TxIn[0].PreviousOutPoint: {
			Value:    5000,
			PkScript: pkScript,
		},
		tx.TxIn[1].PreviousOutPoint: {
			Value:    5000,
			PkScript: pkScript,
		},
	}
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)

	// Create a mock signer.
	lnd := test.NewMockLnd()
	signer := lnd.Signer

	// Create a minimal manager.
	m := &Manager{
		cfg: &ManagerConfig{
			Signer: signer,
		},
	}

	// Call signMusig2Tx - it should error because the second transaction
	// input's outpoint is not in depositsToIdx map.
	//
	// The function should validate that every transaction input has a
	// corresponding entry in depositsToIdx before attempting to sign.
	ctx := context.Background()
	_, err := m.signMusig2Tx(
		ctx, prevOutFetcher, signer, tx, sessions, sigInfo,
		depositsToIdx,
	)

	// Expect an error indicating the missing outpoint.
	require.ErrorContains(t, err, "tx outpoint not in deposit index map")
}

// TestCalculateWithdrawalTxValues tests various edge cases in withdrawal
// transaction value calculations.
func TestCalculateWithdrawalTxValues(t *testing.T) {
	t.Parallel()

	// Create a taproot address for withdrawal.
	taprootAddr, err := btcutil.NewAddressTaproot(
		make([]byte, 32), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	// Standard fee rate for testing.
	feeRate := chainfee.SatPerKWeight(1000)

	// Helper to create deposits.
	createDeposit := func(value btcutil.Amount, idx uint32) *deposit.Deposit {
		hash := chainhash.Hash{}
		hash[0] = byte(idx)
		return &deposit.Deposit{
			OutPoint: wire.OutPoint{
				Hash:  hash,
				Index: idx,
			},
			Value: value,
		}
	}

	tests := []struct {
		name           string
		deposits       []*deposit.Deposit
		localAmount    btcutil.Amount
		feeRate        chainfee.SatPerKWeight
		withdrawAddr   btcutil.Address
		commitmentType lnrpc.CommitmentType
		expectedErr    string
		expectDustFee  bool // change is dust, given to miners
	}{
		{
			name: "neither address nor commitment type specified",
			deposits: []*deposit.Deposit{
				createDeposit(100000, 0),
			},
			localAmount:    0,
			feeRate:        feeRate,
			withdrawAddr:   nil,
			commitmentType: lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			expectedErr:    "either address or commitment type must be specified",
		},
		{
			name: "change is dust - given to miners",
			deposits: []*deposit.Deposit{
				createDeposit(100000, 0),
			},
			// Set localAmount such that change after feeWithChange
			// would be dust, but change after feeWithoutChange >= 0.
			// This triggers case: change-feeWithoutChange >= 0
			localAmount:    99300, // Leaves ~700 sats which is dust
			feeRate:        feeRate,
			withdrawAddr:   taprootAddr,
			commitmentType: lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			expectedErr:    "",
			expectDustFee:  true,
		},
		{
			name: "insufficient funds after dust and fee",
			deposits: []*deposit.Deposit{
				createDeposit(1000, 0),
			},
			localAmount:    900,
			feeRate:        feeRate,
			withdrawAddr:   taprootAddr,
			commitmentType: lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			expectedErr:    "doesn't cover for fees",
		},
		{
			name: "negative change after fees",
			deposits: []*deposit.Deposit{
				createDeposit(10000, 0),
			},
			localAmount:    15000,
			feeRate:        feeRate,
			withdrawAddr:   taprootAddr,
			commitmentType: lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			expectedErr:    "doesn't cover for fees",
		},
		{
			name: "min channel size guard - below minimum",
			deposits: []*deposit.Deposit{
				createDeposit(funding.MinChanFundingSize-10, 0),
			},
			localAmount:    0,
			feeRate:        feeRate,
			withdrawAddr:   nil,
			commitmentType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
			expectedErr: "is lower than the minimum channel " +
				"funding size",
		},
		{
			name: "min channel size guard - exactly minimum",
			deposits: []*deposit.Deposit{
				createDeposit(funding.MinChanFundingSize+1000, 0),
			},
			localAmount:    0,
			feeRate:        feeRate,
			withdrawAddr:   nil,
			commitmentType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
			expectedErr:    "",
		},
		{
			name: "withdrawal amount below dust limit",
			deposits: []*deposit.Deposit{
				createDeposit(400, 0),
			},
			localAmount:    0,
			feeRate:        feeRate,
			withdrawAddr:   taprootAddr,
			commitmentType: lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			expectedErr:    "below dust limit",
		},
		{
			name: "change higher than input value",
			deposits: []*deposit.Deposit{
				createDeposit(10000, 0),
				createDeposit(5000, 1),
			},
			localAmount:    5000,
			feeRate:        chainfee.SatPerKWeight(100),
			withdrawAddr:   taprootAddr,
			commitmentType: lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			expectedErr:    "change amount",
		},
		{
			name: "successful withdrawal with change",
			deposits: []*deposit.Deposit{
				createDeposit(100000, 0),
			},
			localAmount:    50000,
			feeRate:        feeRate,
			withdrawAddr:   taprootAddr,
			commitmentType: lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			expectedErr:    "",
		},
		{
			name: "successful withdrawal no change",
			deposits: []*deposit.Deposit{
				createDeposit(100000, 0),
			},
			localAmount:    0,
			feeRate:        feeRate,
			withdrawAddr:   taprootAddr,
			commitmentType: lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			expectedErr:    "",
		},
		{
			name: "successful channel open above min size",
			deposits: []*deposit.Deposit{
				createDeposit(funding.MinChanFundingSize*2, 0),
			},
			localAmount:    0,
			feeRate:        feeRate,
			withdrawAddr:   nil,
			commitmentType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
			expectedErr:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withdrawAmt, changeAmt, err := CalculateWithdrawalTxValues(
				tc.deposits, tc.localAmount, tc.feeRate,
				tc.withdrawAddr, tc.commitmentType,
			)

			if tc.expectedErr != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.expectedErr)
				return
			}

			require.NoError(t, err)
			require.Greater(t, withdrawAmt, btcutil.Amount(0))
			require.GreaterOrEqual(t, changeAmt, btcutil.Amount(0))

			// Verify that withdrawal amount meets dust threshold.
			dustLimit := lnwallet.DustLimitForSize(input.P2TRSize)
			require.GreaterOrEqual(t, withdrawAmt, dustLimit)

			// If this is a channel open, verify min channel size.
			if tc.commitmentType != lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE {
				require.GreaterOrEqual(
					t, withdrawAmt, funding.MinChanFundingSize,
				)
			}

			// If expecting dust to be given to miners, verify
			// changeAmt is 0.
			if tc.expectDustFee {
				require.Equal(t, btcutil.Amount(0), changeAmt,
					"change should be 0 when dust is given to miners")
			}

			// Verify total accounting: inputs = withdrawal + change + fees.
			totalInputs := btcutil.Amount(0)
			for _, d := range tc.deposits {
				totalInputs += d.Value
			}

			hasChange := changeAmt > 0
			weight, err := WithdrawalTxWeight(
				len(tc.deposits), tc.withdrawAddr,
				tc.commitmentType, hasChange,
			)
			require.NoError(t, err)

			fee := tc.feeRate.FeeForWeight(weight)

			// When dust is given to miners, the "fee" includes both
			// the transaction fee and the dust amount.
			if tc.expectDustFee {
				// Total should equal withdrawal + implicit fee (including dust)
				implicitFee := totalInputs - withdrawAmt - changeAmt
				require.Greater(t, implicitFee, fee,
					"implicit fee should be greater than tx fee when dust is given to miners")
			} else {
				require.Equal(t, totalInputs, withdrawAmt+changeAmt+fee)
			}
		})
	}
}
