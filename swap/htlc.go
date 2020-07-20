package swap

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
)

// HtlcOutputType defines the output type of the htlc that is published.
type HtlcOutputType uint8

const (
	// HtlcP2WSH is a pay-to-witness-script-hash output (segwit only)
	HtlcP2WSH HtlcOutputType = iota

	// HtlcNP2WSH is a nested pay-to-witness-script-hash output that can be
	// paid to be legacy wallets.
	HtlcNP2WSH
)

// ScriptVersion defines the HTLC script version.
type ScriptVersion uint8

const (
	// HtlcV1 refers to the original version of the HTLC script.
	HtlcV1 ScriptVersion = iota
)

// Htlc contains relevant htlc information from the receiver perspective.
type Htlc struct {
	Version     ScriptVersion
	Script      []byte
	PkScript    []byte
	Hash        lntypes.Hash
	OutputType  HtlcOutputType
	ChainParams *chaincfg.Params
	Address     btcutil.Address
	SigScript   []byte
}

var (
	quoteKey [33]byte

	quoteHash lntypes.Hash

	// QuoteHtlc is a template script just used for fee estimation. It uses
	// the maximum value for cltv expiry to get the maximum (worst case)
	// script size.
	QuoteHtlc, _ = NewHtlc(
		HtlcV1,
		^int32(0), quoteKey, quoteKey, quoteHash, HtlcP2WSH,
		&chaincfg.MainNetParams,
	)
)

// String returns the string value of HtlcOutputType.
func (h HtlcOutputType) String() string {
	switch h {
	case HtlcP2WSH:
		return "P2WSH"

	case HtlcNP2WSH:
		return "NP2WSH"

	default:
		return "unknown"
	}
}

// NewHtlc returns a new instance.
func NewHtlc(version ScriptVersion, cltvExpiry int32,
	senderKey, receiverKey [33]byte,
	hash lntypes.Hash, outputType HtlcOutputType,
	chainParams *chaincfg.Params) (*Htlc, error) {

	var (
		err    error
		script []byte
	)

	switch version {
	case HtlcV1:
		script, err = swapHTLCScriptV1(
			cltvExpiry, senderKey, receiverKey, hash,
		)

	default:
		return nil, fmt.Errorf("unknown script version: %v", version)
	}

	if err != nil {
		return nil, err
	}

	p2wshPkScript, err := input.WitnessScriptHash(script)
	if err != nil {
		return nil, err
	}

	var pkScript, sigScript []byte
	var address btcutil.Address

	switch outputType {
	case HtlcNP2WSH:
		// Generate p2sh script for p2wsh (nested).
		p2wshPkScriptHash := sha256.Sum256(p2wshPkScript)
		hash160 := input.Ripemd160H(p2wshPkScriptHash[:])

		builder := txscript.NewScriptBuilder()

		builder.AddOp(txscript.OP_HASH160)
		builder.AddData(hash160)
		builder.AddOp(txscript.OP_EQUAL)

		pkScript, err = builder.Script()
		if err != nil {
			return nil, err
		}

		// Generate a valid sigScript that will allow us to spend the
		// p2sh output. The sigScript will contain only a single push of
		// the p2wsh witness program corresponding to the matching
		// public key of this address.
		sigScript, err = txscript.NewScriptBuilder().
			AddData(p2wshPkScript).
			Script()
		if err != nil {
			return nil, err
		}

		address, err = btcutil.NewAddressScriptHash(
			p2wshPkScript, chainParams,
		)
		if err != nil {
			return nil, err
		}

	case HtlcP2WSH:
		pkScript = p2wshPkScript

		address, err = btcutil.NewAddressWitnessScriptHash(
			p2wshPkScript[2:],
			chainParams,
		)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unknown output type")
	}

	return &Htlc{
		Hash:        hash,
		Version:     version,
		Script:      script,
		PkScript:    pkScript,
		OutputType:  outputType,
		ChainParams: chainParams,
		Address:     address,
		SigScript:   sigScript,
	}, nil
}

// SwapHTLCScriptV1 returns the on-chain HTLC witness script.
//
// OP_SIZE 32 OP_EQUAL
// OP_IF
//    OP_HASH160 <ripemd160(swapHash)> OP_EQUALVERIFY
//    <receiverHtlcKey>
// OP_ELSE
//    OP_DROP
//    <cltv timeout> OP_CHECKLOCKTIMEVERIFY OP_DROP
//    <senderHtlcKey>
// OP_ENDIF
// OP_CHECKSIG
func swapHTLCScriptV1(cltvExpiry int32, senderHtlcKey,
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

// AddSuccessToEstimator adds a successful spend to a weight estimator.
func (h *Htlc) AddSuccessToEstimator(estimator *input.TxWeightEstimator) {
	// Calculate maximum success witness size
	//
	// - number_of_witness_elements: 1 byte
	// - receiver_sig_length: 1 byte
	// - receiver_sig: 73 bytes
	// - preimage_length: 1 byte
	// - preimage: 33 bytes
	// - witness_script_length: 1 byte
	// - witness_script: len(script) bytes
	maxSuccessWitnessSize := 1 + 1 + 73 + 1 + 33 + 1 + len(h.Script)

	switch h.OutputType {
	case HtlcP2WSH:
		estimator.AddWitnessInput(maxSuccessWitnessSize)

	case HtlcNP2WSH:
		estimator.AddNestedP2WSHInput(maxSuccessWitnessSize)
	}
}

// AddTimeoutToEstimator adds a timeout spend to a weight estimator.
func (h *Htlc) AddTimeoutToEstimator(estimator *input.TxWeightEstimator) {
	// Calculate maximum timeout witness size
	//
	// - number_of_witness_elements: 1 byte
	// - sender_sig_length: 1 byte
	// - sender_sig: 73 bytes
	// - zero_length: 1 byte
	// - zero: 1 byte
	// - witness_script_length: 1 byte
	// - witness_script: len(script) bytes
	maxTimeoutWitnessSize := 1 + 1 + 73 + 1 + 1 + 1 + len(h.Script)

	switch h.OutputType {
	case HtlcP2WSH:
		estimator.AddWitnessInput(maxTimeoutWitnessSize)

	case HtlcNP2WSH:
		estimator.AddNestedP2WSHInput(maxTimeoutWitnessSize)
	}
}
