package withdraw

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func init() {
	// Initialize logger for tests to avoid nil pointer panics.
	UseLogger(btclog.Disabled)
}

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

// TestSignMusig2Tx_InvalidServerSigningInfo tests that malformed server
// signing data is rejected before it is passed to the signer.
func TestSignMusig2Tx_InvalidServerSigningInfo(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	outpoint := wire.OutPoint{
		Hash:  [32]byte{1},
		Index: 0,
	}
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: outpoint,
	})

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

	depositKey := outpoint.String()
	sessions := map[string]*input.MuSig2SessionInfo{
		depositKey: {
			SessionID: [32]byte{1},
		},
	}
	depositsToIdx := map[string]int{
		depositKey: 0,
	}
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{
			outpoint: {
				Value:    5000,
				PkScript: pkScript,
			},
		},
	)

	validNonce := make([]byte, musig2.PubNonceSize)
	validSig := make([]byte, input.MuSig2PartialSigSize)

	tests := []struct {
		name        string
		signingInfo *swapserverrpc.ServerPsbtWithdrawSigningInfo
		errContains string
	}{
		{
			name:        "nil signing info",
			signingInfo: nil,
			errContains: "missing signing info",
		},
		{
			name: "invalid nonce length",
			signingInfo: &swapserverrpc.ServerPsbtWithdrawSigningInfo{
				Nonce: validNonce[:musig2.PubNonceSize-1],
				Sig:   validSig,
			},
			errContains: "invalid nonce length",
		},
		{
			name: "invalid partial signature length",
			signingInfo: &swapserverrpc.ServerPsbtWithdrawSigningInfo{
				Nonce: validNonce,
				Sig:   validSig[:input.MuSig2PartialSigSize-1],
			},
			errContains: "invalid partial signature length",
		},
	}

	lnd := test.NewMockLnd()
	m := &Manager{
		cfg: &ManagerConfig{
			Signer: lnd.Signer,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sigInfo := map[string]*swapserverrpc.ServerPsbtWithdrawSigningInfo{
				depositKey: tc.signingInfo,
			}

			_, err := m.signMusig2Tx(
				context.Background(), prevOutFetcher, lnd.Signer,
				tx.Copy(), sessions, sigInfo, depositsToIdx,
			)
			require.ErrorContains(t, err, tc.errContains)
		})
	}
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

// ---------------------------------------------------------------------------
// Mock types for handleWithdrawal tests
// ---------------------------------------------------------------------------

// mockChainNotifier is a mock implementation of lndclient.ChainNotifierClient.
type mockChainNotifier struct {
	mock.Mock

	mu sync.Mutex

	// Track call counts for conditional behavior.
	spendNtfnCalls int
	confNtfnCalls  int
}

func (m *mockChainNotifier) RegisterSpendNtfn(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint int32,
	opts ...lndclient.NotifierOption) (chan *chainntnfs.SpendDetail,
	chan error, error) {

	m.mu.Lock()
	m.spendNtfnCalls++
	callNum := m.spendNtfnCalls
	m.mu.Unlock()

	args := m.Called(ctx, outpoint, pkScript, heightHint, callNum)

	spendChan := args.Get(0)
	errChan := args.Get(1)

	var sc chan *chainntnfs.SpendDetail
	var ec chan error

	if spendChan != nil {
		sc = spendChan.(chan *chainntnfs.SpendDetail)
	}
	if errChan != nil {
		ec = errChan.(chan error)
	}

	return sc, ec, args.Error(2)
}

func (m *mockChainNotifier) RegisterConfirmationsNtfn(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs, heightHint int32,
	opts ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

	m.mu.Lock()
	m.confNtfnCalls++
	callNum := m.confNtfnCalls
	m.mu.Unlock()

	args := m.Called(ctx, txid, pkScript, numConfs, heightHint, callNum)

	confChan := args.Get(0)
	errChan := args.Get(1)

	var cc chan *chainntnfs.TxConfirmation
	var ec chan error

	if confChan != nil {
		cc = confChan.(chan *chainntnfs.TxConfirmation)
	}
	if errChan != nil {
		ec = errChan.(chan error)
	}

	return cc, ec, args.Error(2)
}

func (m *mockChainNotifier) RegisterBlockEpochNtfn(ctx context.Context) (
	chan int32, chan error, error) {

	args := m.Called(ctx)

	blockChan := args.Get(0)
	errChan := args.Get(1)

	var bc chan int32
	var ec chan error

	if blockChan != nil {
		bc = blockChan.(chan int32)
	}
	if errChan != nil {
		ec = errChan.(chan error)
	}

	return bc, ec, args.Error(2)
}

func (m *mockChainNotifier) RawClientWithMacAuth(ctx context.Context) (
	context.Context, time.Duration, chainrpc.ChainNotifierClient) {

	return ctx, 0, nil
}

// ---------------------------------------------------------------------------
// handleWithdrawal tests
// ---------------------------------------------------------------------------

// testDeposit creates a test deposit with the given parameters.
func testDeposit(hashByte byte, value btcutil.Amount,
	confHeight uint32) *deposit.Deposit {

	hash := chainhash.Hash{}
	hash[0] = hashByte
	return &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  hash,
			Index: 0,
		},
		Value:              value,
		ConfirmationHeight: int64(confHeight),
	}
}

// TestHandleWithdrawal_HappyPath tests the successful withdrawal flow up to
// confirmation registration. Full flow including storage persistence requires
// integration tests since Store is a concrete type (*SqlStore).
//
// Tests: spend detected -> confirmation registration succeeds.
func TestHandleWithdrawal_HappyPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create test data.
	deposits := []*deposit.Deposit{testDeposit(1, 100000, 100)}
	txHash := chainhash.Hash{0x01}
	spenderTxHash := chainhash.Hash{0x02}
	withdrawalPkScript := []byte{0x51, 0x20}
	staticAddrPkScript := []byte{0x51, 0x21}

	// Create channels for notifications.
	blockChan := make(chan int32, 1)
	blockErrChan := make(chan error, 1)
	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	spendErrChan := make(chan error, 1)
	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	confErrChan := make(chan error, 1)

	// Set up mocks.
	mockNotifier := &mockChainNotifier{}

	mockNotifier.On("RegisterBlockEpochNtfn", mock.Anything).
		Return(blockChan, blockErrChan, nil)
	mockNotifier.On("RegisterSpendNtfn", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, 1).
		Return(spendChan, spendErrChan, nil)
	mockNotifier.On("RegisterConfirmationsNtfn", mock.Anything,
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, 1).
		Return(confChan, confErrChan, nil)

	// Create manager with mocks.
	// Note: Store is nil, so we can't test the full flow including
	// UpdateWithdrawal. That requires integration tests.
	m := &Manager{
		cfg: &ManagerConfig{
			ChainNotifier: mockNotifier,
		},
		finalizedWithdrawalTxns: make(map[chainhash.Hash]*wire.MsgTx),
	}
	m.initiationHeight.Store(100)

	// Call handleWithdrawal (spawns goroutine).
	m.handleWithdrawal(ctx, deposits, txHash, withdrawalPkScript,
		staticAddrPkScript)

	// Send spend notification.
	spendChan <- &chainntnfs.SpendDetail{
		SpenderTxHash:  &spenderTxHash,
		SpendingHeight: 105,
	}

	// Give time for spend to be processed and conf registration to occur.
	time.Sleep(100 * time.Millisecond)

	// Verify spend and confirmation registration happened.
	mockNotifier.AssertCalled(t, "RegisterSpendNtfn", mock.Anything,
		mock.Anything, mock.Anything, mock.Anything, 1)
	mockNotifier.AssertCalled(t, "RegisterConfirmationsNtfn", mock.Anything,
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, 1)

	// Cancel to clean up goroutine (we're not testing full confirmation
	// flow since Store is nil).
	cancel()
}

// TestHandleWithdrawal_SpendRegistrationRetry tests that spend registration
// is retried on next block when it fails.
func TestHandleWithdrawal_SpendRegistrationRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create test data.
	deposits := []*deposit.Deposit{testDeposit(1, 100000, 100)}
	txHash := chainhash.Hash{0x01}
	spenderTxHash := chainhash.Hash{0x02}
	withdrawalPkScript := []byte{0x51, 0x20}
	staticAddrPkScript := []byte{0x51, 0x21}

	// Create channels.
	blockChan := make(chan int32, 2)
	blockErrChan := make(chan error, 1)
	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	spendErrChan := make(chan error, 1)
	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	confErrChan := make(chan error, 1)

	// Set up mocks.
	mockNotifier := &mockChainNotifier{}

	mockNotifier.On("RegisterBlockEpochNtfn", mock.Anything).
		Return(blockChan, blockErrChan, nil)

	// First call fails, second succeeds.
	mockNotifier.On("RegisterSpendNtfn", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, 1).
		Return(nil, nil, errors.New("temporary failure"))
	mockNotifier.On("RegisterSpendNtfn", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, 2).
		Return(spendChan, spendErrChan, nil)

	mockNotifier.On("RegisterConfirmationsNtfn", mock.Anything,
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, 1).
		Return(confChan, confErrChan, nil)

	// Create manager.
	m := &Manager{
		cfg: &ManagerConfig{
			ChainNotifier: mockNotifier,
		},
		finalizedWithdrawalTxns: make(map[chainhash.Hash]*wire.MsgTx),
	}
	m.initiationHeight.Store(100)

	// Call handleWithdrawal.
	m.handleWithdrawal(ctx, deposits, txHash, withdrawalPkScript,
		staticAddrPkScript)

	// Send block to trigger retry.
	blockChan <- 101

	// Give time for retry.
	time.Sleep(50 * time.Millisecond)

	// Send spend notification (on successful registration).
	spendChan <- &chainntnfs.SpendDetail{
		SpenderTxHash:  &spenderTxHash,
		SpendingHeight: 105,
	}

	// Give time for confirmation registration.
	time.Sleep(100 * time.Millisecond)

	// Verify both spend registrations were attempted.
	mockNotifier.AssertNumberOfCalls(t, "RegisterSpendNtfn", 2)
	// Verify we reached confirmation registration (retry worked).
	mockNotifier.AssertCalled(t, "RegisterConfirmationsNtfn", mock.Anything,
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, 1)

	cancel()
}

// TestHandleWithdrawal_ConfirmationRegistrationRetry tests that confirmation
// registration is retried on next block when it fails.
func TestHandleWithdrawal_ConfirmationRegistrationRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create test data.
	deposits := []*deposit.Deposit{testDeposit(1, 100000, 100)}
	txHash := chainhash.Hash{0x01}
	spenderTxHash := chainhash.Hash{0x02}
	withdrawalPkScript := []byte{0x51, 0x20}
	staticAddrPkScript := []byte{0x51, 0x21}

	// Create channels.
	blockChan := make(chan int32, 2)
	blockErrChan := make(chan error, 1)
	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	spendErrChan := make(chan error, 1)
	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	confErrChan := make(chan error, 1)

	// Set up mocks.
	mockNotifier := &mockChainNotifier{}

	mockNotifier.On("RegisterBlockEpochNtfn", mock.Anything).
		Return(blockChan, blockErrChan, nil)
	mockNotifier.On("RegisterSpendNtfn", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, 1).
		Return(spendChan, spendErrChan, nil)

	// First confirmation registration fails, second succeeds.
	mockNotifier.On("RegisterConfirmationsNtfn", mock.Anything,
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, 1).
		Return(nil, nil, errors.New("temporary failure"))
	mockNotifier.On("RegisterConfirmationsNtfn", mock.Anything,
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, 2).
		Return(confChan, confErrChan, nil)

	// Create manager.
	m := &Manager{
		cfg: &ManagerConfig{
			ChainNotifier: mockNotifier,
		},
		finalizedWithdrawalTxns: make(map[chainhash.Hash]*wire.MsgTx),
	}
	m.initiationHeight.Store(100)

	// Call handleWithdrawal.
	m.handleWithdrawal(ctx, deposits, txHash, withdrawalPkScript,
		staticAddrPkScript)

	// Send spend notification.
	spendChan <- &chainntnfs.SpendDetail{
		SpenderTxHash:  &spenderTxHash,
		SpendingHeight: 105,
	}

	// Give time for first conf registration to fail.
	time.Sleep(50 * time.Millisecond)

	// Send block to trigger retry.
	blockChan <- 106

	// Give time for retry.
	time.Sleep(100 * time.Millisecond)

	// Verify both confirmation registrations were attempted.
	mockNotifier.AssertNumberOfCalls(t, "RegisterConfirmationsNtfn", 2)

	cancel()
}

// TestHandleWithdrawal_ContextCanceled tests that the goroutine exits cleanly
// when the context is canceled.
func TestHandleWithdrawal_ContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	// Create test data.
	deposits := []*deposit.Deposit{testDeposit(1, 100000, 100)}
	txHash := chainhash.Hash{0x01}
	withdrawalPkScript := []byte{0x51, 0x20}
	staticAddrPkScript := []byte{0x51, 0x21}

	// Create channels.
	blockChan := make(chan int32, 1)
	blockErrChan := make(chan error, 1)
	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	spendErrChan := make(chan error, 1)

	// Set up mocks.
	mockNotifier := &mockChainNotifier{}

	mockNotifier.On("RegisterBlockEpochNtfn", mock.Anything).
		Return(blockChan, blockErrChan, nil)
	mockNotifier.On("RegisterSpendNtfn", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, 1).
		Return(spendChan, spendErrChan, nil)

	// Create manager.
	m := &Manager{
		cfg: &ManagerConfig{
			ChainNotifier: mockNotifier,
		},
		finalizedWithdrawalTxns: make(map[chainhash.Hash]*wire.MsgTx),
	}
	m.initiationHeight.Store(100)

	// Call handleWithdrawal.
	m.handleWithdrawal(ctx, deposits, txHash, withdrawalPkScript,
		staticAddrPkScript)

	// Give time for goroutine to start.
	time.Sleep(50 * time.Millisecond)

	// Cancel context.
	cancel()

	// Give time for goroutine to exit.
	time.Sleep(100 * time.Millisecond)

	// If we get here without hanging, the test passes.
	// The goroutine should have exited cleanly.
	mockNotifier.AssertExpectations(t)
}

// TestHandleWithdrawal_BlockChannelClosed tests that the goroutine exits
// cleanly when the block channel is closed.
func TestHandleWithdrawal_BlockChannelClosed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create test data.
	deposits := []*deposit.Deposit{testDeposit(1, 100000, 100)}
	txHash := chainhash.Hash{0x01}
	withdrawalPkScript := []byte{0x51, 0x20}
	staticAddrPkScript := []byte{0x51, 0x21}

	// Create channels.
	blockChan := make(chan int32)
	blockErrChan := make(chan error, 1)

	// Set up mocks - spend registration fails to force retry path.
	mockNotifier := &mockChainNotifier{}

	mockNotifier.On("RegisterBlockEpochNtfn", mock.Anything).
		Return(blockChan, blockErrChan, nil)
	mockNotifier.On("RegisterSpendNtfn", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, 1).
		Return(nil, nil, errors.New("temporary failure"))

	// Create manager.
	m := &Manager{
		cfg: &ManagerConfig{
			ChainNotifier: mockNotifier,
		},
		finalizedWithdrawalTxns: make(map[chainhash.Hash]*wire.MsgTx),
	}
	m.initiationHeight.Store(100)

	// Call handleWithdrawal.
	m.handleWithdrawal(ctx, deposits, txHash, withdrawalPkScript,
		staticAddrPkScript)

	// Give time for goroutine to hit retry wait.
	time.Sleep(50 * time.Millisecond)

	// Close block channel.
	close(blockChan)

	// Give time for goroutine to exit.
	time.Sleep(100 * time.Millisecond)

	// If we get here without hanging, the test passes.
	mockNotifier.AssertExpectations(t)
}

// TestHandleWithdrawal_SpendErrorReregisters tests that an error on the spend
// error channel causes re-registration.
func TestHandleWithdrawal_SpendErrorReregisters(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create test data.
	deposits := []*deposit.Deposit{testDeposit(1, 100000, 100)}
	txHash := chainhash.Hash{0x01}
	spenderTxHash := chainhash.Hash{0x02}
	withdrawalPkScript := []byte{0x51, 0x20}
	staticAddrPkScript := []byte{0x51, 0x21}

	// Create channels.
	blockChan := make(chan int32, 1)
	blockErrChan := make(chan error, 1)
	spendChan1 := make(chan *chainntnfs.SpendDetail, 1)
	spendErrChan1 := make(chan error, 1)
	spendChan2 := make(chan *chainntnfs.SpendDetail, 1)
	spendErrChan2 := make(chan error, 1)
	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	confErrChan := make(chan error, 1)

	// Set up mocks.
	mockNotifier := &mockChainNotifier{}

	mockNotifier.On("RegisterBlockEpochNtfn", mock.Anything).
		Return(blockChan, blockErrChan, nil)

	// First spend registration succeeds but will send error.
	mockNotifier.On("RegisterSpendNtfn", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, 1).
		Return(spendChan1, spendErrChan1, nil)
	// Second spend registration succeeds normally.
	mockNotifier.On("RegisterSpendNtfn", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, 2).
		Return(spendChan2, spendErrChan2, nil)

	mockNotifier.On("RegisterConfirmationsNtfn", mock.Anything,
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, 1).
		Return(confChan, confErrChan, nil)

	// Create manager.
	m := &Manager{
		cfg: &ManagerConfig{
			ChainNotifier: mockNotifier,
		},
		finalizedWithdrawalTxns: make(map[chainhash.Hash]*wire.MsgTx),
	}
	m.initiationHeight.Store(100)

	// Call handleWithdrawal.
	m.handleWithdrawal(ctx, deposits, txHash, withdrawalPkScript,
		staticAddrPkScript)

	// Give time for goroutine to register.
	time.Sleep(50 * time.Millisecond)

	// Send error on first spend channel to trigger re-registration.
	spendErrChan1 <- errors.New("spend notification error")

	// Give time for re-registration.
	time.Sleep(50 * time.Millisecond)

	// Send spend on second channel.
	spendChan2 <- &chainntnfs.SpendDetail{
		SpenderTxHash:  &spenderTxHash,
		SpendingHeight: 105,
	}

	// Give time for confirmation registration.
	time.Sleep(100 * time.Millisecond)

	// Verify both spend registrations were called.
	mockNotifier.AssertNumberOfCalls(t, "RegisterSpendNtfn", 2)
	// Verify we reached confirmation registration.
	mockNotifier.AssertCalled(t, "RegisterConfirmationsNtfn", mock.Anything,
		mock.Anything, mock.Anything, mock.Anything, mock.Anything, 1)

	cancel()
}
