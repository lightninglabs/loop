package withdraw

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
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
