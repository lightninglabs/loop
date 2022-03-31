package swap

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
)

// ErrNoSharedKey is returned when a script version does not support use of a
// shared key.
var ErrNoSharedKey = errors.New("shared key not supported for script version")

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

	// HtlcV2 refers to the improved version of the HTLC script.
	HtlcV2
)

// htlcScript defines an interface for the different HTLC implementations.
type HtlcScript interface {
	// genSuccessWitness returns the success script to spend this htlc with
	// the preimage.
	genSuccessWitness(receiverSig []byte, preimage lntypes.Preimage) wire.TxWitness

	// GenTimeoutWitness returns the timeout script to spend this htlc after
	// timeout.
	GenTimeoutWitness(senderSig []byte) wire.TxWitness

	// IsSuccessWitness checks whether the given stack is valid for
	// redeeming the htlc.
	IsSuccessWitness(witness wire.TxWitness) bool

	// Script returns the htlc script.
	Script() []byte

	// MaxSuccessWitnessSize returns the maximum witness size for the
	// success case witness.
	MaxSuccessWitnessSize() int

	// MaxTimeoutWitnessSize returns the maximum witness size for the
	// timeout case witness.
	MaxTimeoutWitnessSize() int

	// SuccessSequence returns the sequence to spend this htlc in the
	// success case.
	SuccessSequence() uint32
}

// Htlc contains relevant htlc information from the receiver perspective.
type Htlc struct {
	HtlcScript

	Version     ScriptVersion
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
		HtlcV2,
		^int32(0), quoteKey, quoteKey, nil, quoteHash, HtlcP2WSH,
		&chaincfg.MainNetParams,
	)

	ErrInvalidScriptVersion = fmt.Errorf("invalid script version")
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
	senderKey, receiverKey [33]byte, sharedKey *btcec.PublicKey,
	hash lntypes.Hash, outputType HtlcOutputType,
	chainParams *chaincfg.Params) (*Htlc, error) {

	var (
		err  error
		htlc HtlcScript
	)

	switch version {
	case HtlcV1:
		if sharedKey != nil {
			return nil, ErrNoSharedKey
		}

		htlc, err = newHTLCScriptV1(
			cltvExpiry, senderKey, receiverKey, hash,
		)

	case HtlcV2:
		if sharedKey != nil {
			return nil, ErrNoSharedKey
		}

		htlc, err = newHTLCScriptV2(
			cltvExpiry, senderKey, receiverKey, hash,
		)

	default:
		return nil, ErrInvalidScriptVersion
	}

	if err != nil {
		return nil, err
	}

	p2wshPkScript, err := input.WitnessScriptHash(htlc.Script())
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
		HtlcScript:  htlc,
		Hash:        hash,
		Version:     version,
		PkScript:    pkScript,
		OutputType:  outputType,
		ChainParams: chainParams,
		Address:     address,
		SigScript:   sigScript,
	}, nil
}

// GenSuccessWitness returns the success script to spend this htlc with
// the preimage.
func (h *Htlc) GenSuccessWitness(receiverSig []byte,
	preimage lntypes.Preimage) (wire.TxWitness, error) {

	if h.Hash != preimage.Hash() {
		return nil, errors.New("preimage doesn't match hash")
	}

	return h.genSuccessWitness(receiverSig, preimage), nil
}

// AddSuccessToEstimator adds a successful spend to a weight estimator.
func (h *Htlc) AddSuccessToEstimator(estimator *input.TxWeightEstimator) {
	maxSuccessWitnessSize := h.MaxSuccessWitnessSize()

	switch h.OutputType {
	case HtlcP2WSH:
		estimator.AddWitnessInput(maxSuccessWitnessSize)

	case HtlcNP2WSH:
		estimator.AddNestedP2WSHInput(maxSuccessWitnessSize)
	}
}

// AddTimeoutToEstimator adds a timeout spend to a weight estimator.
func (h *Htlc) AddTimeoutToEstimator(estimator *input.TxWeightEstimator) {
	maxTimeoutWitnessSize := h.MaxTimeoutWitnessSize()

	switch h.OutputType {
	case HtlcP2WSH:
		estimator.AddWitnessInput(maxTimeoutWitnessSize)

	case HtlcNP2WSH:
		estimator.AddNestedP2WSHInput(maxTimeoutWitnessSize)
	}
}

// HtlcScriptV1 encapsulates the htlc v1 script.
type HtlcScriptV1 struct {
	script []byte
}

// newHTLCScriptV1 constructs an HtlcScript with the HTLC V1 witness script.
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
func newHTLCScriptV1(cltvExpiry int32, senderHtlcKey,
	receiverHtlcKey [33]byte, swapHash lntypes.Hash) (*HtlcScriptV1, error) {

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

	script, err := builder.Script()
	if err != nil {
		return nil, err
	}

	return &HtlcScriptV1{
		script: script,
	}, nil
}

// genSuccessWitness returns the success script to spend this htlc with
// the preimage.
func (h *HtlcScriptV1) genSuccessWitness(receiverSig []byte,
	preimage lntypes.Preimage) wire.TxWitness {

	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = append(receiverSig, byte(txscript.SigHashAll))
	witnessStack[1] = preimage[:]
	witnessStack[2] = h.script

	return witnessStack
}

// GenTimeoutWitness returns the timeout script to spend this htlc after
// timeout.
func (h *HtlcScriptV1) GenTimeoutWitness(senderSig []byte) wire.TxWitness {

	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = append(senderSig, byte(txscript.SigHashAll))
	witnessStack[1] = []byte{0}
	witnessStack[2] = h.script

	return witnessStack
}

// IsSuccessWitness checks whether the given stack is valid for redeeming the
// htlc.
func (h *HtlcScriptV1) IsSuccessWitness(witness wire.TxWitness) bool {
	if len(witness) != 3 {
		return false
	}

	isTimeoutTx := bytes.Equal([]byte{0}, witness[1])
	return !isTimeoutTx
}

// Script returns the htlc script.
func (h *HtlcScriptV1) Script() []byte {
	return h.script
}

// MaxSuccessWitnessSize returns the maximum success witness size.
func (h *HtlcScriptV1) MaxSuccessWitnessSize() int {
	// Calculate maximum success witness size
	//
	// - number_of_witness_elements: 1 byte
	// - receiver_sig_length: 1 byte
	// - receiver_sig: 73 bytes
	// - preimage_length: 1 byte
	// - preimage: 32 bytes
	// - witness_script_length: 1 byte
	// - witness_script: len(script) bytes
	return 1 + 1 + 73 + 1 + 32 + 1 + len(h.script)
}

// MaxTimeoutWitnessSize return the maximum timeout witness size.
func (h *HtlcScriptV1) MaxTimeoutWitnessSize() int {
	// Calculate maximum timeout witness size
	//
	// - number_of_witness_elements: 1 byte
	// - sender_sig_length: 1 byte
	// - sender_sig: 73 bytes
	// - zero_length: 1 byte
	// - zero: 1 byte
	// - witness_script_length: 1 byte
	// - witness_script: len(script) bytes
	return 1 + 1 + 73 + 1 + 1 + 1 + len(h.script)
}

// SuccessSequence returns the sequence to spend this htlc in the success case.
func (h *HtlcScriptV1) SuccessSequence() uint32 {
	return 0
}

// HtlcScriptV2 encapsulates the htlc v2 script.
type HtlcScriptV2 struct {
	script    []byte
	senderKey [33]byte
}

// newHTLCScriptV2 construct an HtlcScipt with the HTLC V2 witness script.
//
// <receiverHtlcKey> OP_CHECKSIG OP_NOTIF
//   OP_DUP OP_HASH160 <HASH160(senderHtlcKey)> OP_EQUALVERIFY OP_CHECKSIGVERIFY
//   <cltv timeout> OP_CHECKLOCKTIMEVERIFY
// OP_ELSE
//   OP_SIZE <20> OP_EQUALVERIFY OP_HASH160 <ripemd(swapHash)> OP_EQUALVERIFY 1
//   OP_CHECKSEQUENCEVERIFY
// OP_ENDIF
func newHTLCScriptV2(cltvExpiry int32, senderHtlcKey,
	receiverHtlcKey [33]byte, swapHash lntypes.Hash) (*HtlcScriptV2, error) {

	builder := txscript.NewScriptBuilder()
	builder.AddData(receiverHtlcKey[:])
	builder.AddOp(txscript.OP_CHECKSIG)

	builder.AddOp(txscript.OP_NOTIF)

	builder.AddOp(txscript.OP_DUP)
	builder.AddOp(txscript.OP_HASH160)
	senderHtlcKeyHash := sha256.Sum256(senderHtlcKey[:])
	builder.AddData(input.Ripemd160H(senderHtlcKeyHash[:]))

	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)

	builder.AddInt64(int64(cltvExpiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)

	builder.AddOp(txscript.OP_ELSE)

	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(0x20)
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(input.Ripemd160H(swapHash[:]))
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOp(txscript.OP_1)

	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	builder.AddOp(txscript.OP_ENDIF)

	script, err := builder.Script()
	if err != nil {
		return nil, err
	}

	return &HtlcScriptV2{
		script:    script,
		senderKey: senderHtlcKey,
	}, nil
}

// genSuccessWitness returns the success script to spend this htlc with
// the preimage.
func (h *HtlcScriptV2) genSuccessWitness(receiverSig []byte,
	preimage lntypes.Preimage) wire.TxWitness {

	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = preimage[:]
	witnessStack[1] = append(receiverSig, byte(txscript.SigHashAll))
	witnessStack[2] = h.script

	return witnessStack
}

// IsSuccessWitness checks whether the given stack is valid for redeeming the
// htlc.
func (h *HtlcScriptV2) IsSuccessWitness(witness wire.TxWitness) bool {
	isTimeoutTx := len(witness) == 4

	return !isTimeoutTx
}

// GenTimeoutWitness returns the timeout script to spend this htlc after
// timeout.
func (h *HtlcScriptV2) GenTimeoutWitness(senderSig []byte) wire.TxWitness {

	witnessStack := make(wire.TxWitness, 4)
	witnessStack[0] = append(senderSig, byte(txscript.SigHashAll))
	witnessStack[1] = h.senderKey[:]
	witnessStack[2] = []byte{}
	witnessStack[3] = h.script

	return witnessStack
}

// Script returns the htlc script.
func (h *HtlcScriptV2) Script() []byte {
	return h.script
}

// MaxSuccessWitnessSize returns maximum success witness size.
func (h *HtlcScriptV2) MaxSuccessWitnessSize() int {
	// Calculate maximum success witness size
	//
	// - number_of_witness_elements: 1 byte
	// - receiver_sig_length: 1 byte
	// - receiver_sig: 73 bytes
	// - preimage_length: 1 byte
	// - preimage: 32 bytes
	// - witness_script_length: 1 byte
	// - witness_script: len(script) bytes
	return 1 + 1 + 73 + 1 + 32 + 1 + len(h.script)
}

// MaxTimeoutWitnessSize returns maximum timeout witness size.
func (h *HtlcScriptV2) MaxTimeoutWitnessSize() int {
	// Calculate maximum timeout witness size
	//
	// - number_of_witness_elements: 1 byte
	// - sender_sig_length: 1 byte
	// - sender_sig: 73 bytes
	// - sender_key_length: 1 byte
	// - sender_key: 33 bytes
	// - zero: 1 byte
	// - witness_script_length: 1 byte
	// - witness_script: len(script) bytes
	return 1 + 1 + 73 + 1 + 33 + 1 + 1 + len(h.script)
}

// SuccessSequence returns the sequence to spend this htlc in the success case.
func (h *HtlcScriptV2) SuccessSequence() uint32 {
	return 1
}
