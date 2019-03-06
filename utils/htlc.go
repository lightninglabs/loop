package utils

import (
	"bytes"
	"errors"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
)

// Htlc contains relevant htlc information from the receiver perspective.
type Htlc struct {
	Script                []byte
	ScriptHash            []byte
	Hash                  lntypes.Hash
	MaxSuccessWitnessSize int
	MaxTimeoutWitnessSize int
}

var (
	quoteKey [33]byte

	quoteHash lntypes.Hash

	// QuoteHtlc is a template script just used for fee estimation. It uses
	// the maximum value for cltv expiry to get the maximum (worst case)
	// script size.
	QuoteHtlc, _ = NewHtlc(
		^int32(0), quoteKey, quoteKey, quoteHash,
	)
)

// NewHtlc returns a new instance.
func NewHtlc(cltvExpiry int32, senderKey, receiverKey [33]byte,
	hash lntypes.Hash) (*Htlc, error) {

	script, err := swapHTLCScript(
		cltvExpiry, senderKey, receiverKey, hash,
	)
	if err != nil {
		return nil, err
	}

	scriptHash, err := input.WitnessScriptHash(script)
	if err != nil {
		return nil, err
	}

	// Calculate maximum success witness size
	//
	// - number_of_witness_elements: 1 byte
	// - receiver_sig_length: 1 byte
	// - receiver_sig: 73 bytes
	// - preimage_length: 1 byte
	// - preimage: 33 bytes
	// - witness_script_length: 1 byte
	// - witness_script: len(script) bytes
	maxSuccessWitnessSize := 1 + 1 + 73 + 1 + 33 + 1 + len(script)

	// Calculate maximum timeout witness size
	//
	// - number_of_witness_elements: 1 byte
	// - sender_sig_length: 1 byte
	// - sender_sig: 73 bytes
	// - zero_length: 1 byte
	// - zero: 1 byte
	// - witness_script_length: 1 byte
	// - witness_script: len(script) bytes
	maxTimeoutWitnessSize := 1 + 1 + 73 + 1 + 1 + 1 + len(script)

	return &Htlc{
		Hash:                  hash,
		Script:                script,
		ScriptHash:            scriptHash,
		MaxSuccessWitnessSize: maxSuccessWitnessSize,
		MaxTimeoutWitnessSize: maxTimeoutWitnessSize,
	}, nil
}

// SwapHTLCScript returns the on-chain HTLC witness script.
//
// OP_SIZE 32 OP_EQUAL
// OP_IF
//    OP_HASH160 <ripemd160(swap_hash)> OP_EQUALVERIFY
//    <recvr key>
// OP_ELSE
//    OP_DROP
//    <cltv timeout> OP_CHECKLOCKTIMEVERIFY OP_DROP
//    <sender key>
// OP_ENDIF
// OP_CHECKSIG
func swapHTLCScript(cltvExpiry int32, senderHtlcKey,
	receiverHtlcKey [33]byte, swapHash lntypes.Hash) ([]byte, error) {

	builder := txscript.NewScriptBuilder()

	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUAL)

	builder.AddOp(txscript.OP_IF)

	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(input.Ripemd160H(swapHash[:]))
	builder.AddOp(txscript.OP_EQUALVERIFY)

	builder.AddData(receiverHtlcKey[:])

	builder.AddOp(txscript.OP_ELSE)

	builder.AddOp(txscript.OP_DROP)

	builder.AddInt64(int64(cltvExpiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	builder.AddData(senderHtlcKey[:])

	builder.AddOp(txscript.OP_ENDIF)

	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}

// Address returns the p2wsh address of the htlc.
func (h *Htlc) Address(chainParams *chaincfg.Params) (
	btcutil.Address, error) {

	// Skip OP_0 and data length.
	return btcutil.NewAddressWitnessScriptHash(
		h.ScriptHash[2:],
		chainParams,
	)
}

// GenSuccessWitness returns the success script to spend this htlc with the
// preimage.
func (h *Htlc) GenSuccessWitness(receiverSig []byte,
	preimage lntypes.Preimage) (wire.TxWitness, error) {

	if h.Hash != preimage.Hash() {
		return nil, errors.New("preimage doesn't match hash")
	}

	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = append(receiverSig, byte(txscript.SigHashAll))
	witnessStack[1] = preimage[:]
	witnessStack[2] = h.Script

	return witnessStack, nil
}

// IsSuccessWitness checks whether the given stack is valid for redeeming the
// htlc.
func (h *Htlc) IsSuccessWitness(witness wire.TxWitness) bool {
	if len(witness) != 3 {
		return false
	}

	isTimeoutTx := bytes.Equal([]byte{0}, witness[1])

	return !isTimeoutTx
}

// GenTimeoutWitness returns the timeout script to spend this htlc after
// timeout.
func (h *Htlc) GenTimeoutWitness(senderSig []byte) (wire.TxWitness, error) {

	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = append(senderSig, byte(txscript.SigHashAll))
	witnessStack[1] = []byte{0}
	witnessStack[2] = h.Script

	return witnessStack, nil
}
