package swap

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// ErrNoSharedKey is returned when a script version does not support
	// use of a shared key.
	ErrNoSharedKey = errors.New("shared key not supported for script " +
		"version")

	// ErrSharedKeyRequired is returned when a script version requires a
	// shared key.
	ErrSharedKeyRequired = errors.New("shared key required")
)

// HtlcOutputType defines the output type of the htlc that is published.
type HtlcOutputType uint8

const (
	// HtlcP2WSH is a pay-to-witness-script-hash output (segwit only)
	HtlcP2WSH HtlcOutputType = iota

	// HtlcNP2WSH is a nested pay-to-witness-script-hash output that can be
	// paid to be legacy wallets.
	HtlcNP2WSH

	// HtlcP2TR is a pay-to-taproot output with three separate spend paths.
	HtlcP2TR
)

// ScriptVersion defines the HTLC script version.
type ScriptVersion uint8

const (
	// HtlcV1 refers to the original version of the HTLC script.
	HtlcV1 ScriptVersion = iota

	// HtlcV2 refers to the improved version of the HTLC script.
	HtlcV2

	// HtlcV3 refers to an upgraded version of HtlcV2 implemented with
	// tapscript.
	HtlcV3
)

// htlcScript defines an interface for the different HTLC implementations.
type HtlcScript interface {
	// genSuccessWitness returns the success script to spend this htlc with
	// the preimage.
	genSuccessWitness(receiverSig []byte, preimage lntypes.Preimage) (wire.TxWitness, error)

	// GenTimeoutWitness returns the timeout script to spend this htlc after
	// timeout.
	GenTimeoutWitness(senderSig []byte) (wire.TxWitness, error)

	// IsSuccessWitness checks whether the given stack is valid for
	// redeeming the htlc.
	IsSuccessWitness(witness wire.TxWitness) bool

	// lockingConditions return the address, pkScript and sigScript (if
	// required) for a htlc script.
	lockingConditions(HtlcOutputType, *chaincfg.Params) (btcutil.Address,
		[]byte, []byte, error)

	// MaxSuccessWitnessSize returns the maximum witness size for the
	// success case witness.
	MaxSuccessWitnessSize() int

	// MaxTimeoutWitnessSize returns the maximum witness size for the
	// timeout case witness.
	MaxTimeoutWitnessSize() int

	// TimeoutScript returns the redeem script required to unlock the htlc
	// after timeout.
	TimeoutScript() []byte

	// SuccessScript returns the redeem script required to unlock the htlc
	// using the preimage.
	SuccessScript() []byte

	// SuccessSequence returns the sequence to spend this htlc in the
	// success case.
	SuccessSequence() uint32

	// SigHash is the signature hash to use for transactions spending from
	// the htlc.
	SigHash() txscript.SigHashType
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

	case HtlcP2TR:
		return "P2TR"

	default:
		return "unknown"
	}
}

// NewHtlc returns a new instance. For V1 and V2 scripts, receiver and sender
// keys are expected to be in ecdsa compressed format. For V3 scripts, keys are
// expected to be raw private key bytes.
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
	case HtlcV3:
		if sharedKey == nil {
			return nil, ErrSharedKeyRequired
		}

		htlc, err = newHTLCScriptV3(
			cltvExpiry, senderKey, receiverKey, sharedKey, hash,
		)

	default:
		return nil, ErrInvalidScriptVersion
	}

	if err != nil {
		return nil, err
	}

	address, pkScript, sigScript, err := htlc.lockingConditions(
		outputType, chainParams,
	)
	if err != nil {
		return nil, fmt.Errorf("could not get address: %w", err)
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

// segwitV0LockingConditions provides the address, pkScript and sigScript (if
// required) for the segwit v0 script and output type provided.
func segwitV0LockingConditions(outputType HtlcOutputType,
	chainParams *chaincfg.Params, script []byte) (btcutil.Address,
	[]byte, []byte, error) {

	switch outputType {
	case HtlcNP2WSH:
		pks, err := input.WitnessScriptHash(script)
		if err != nil {
			return nil, nil, nil, err
		}

		// Generate p2sh script for p2wsh (nested).
		p2wshPkScriptHash := sha256.Sum256(pks)
		hash160 := input.Ripemd160H(p2wshPkScriptHash[:])

		builder := txscript.NewScriptBuilder()

		builder.AddOp(txscript.OP_HASH160)
		builder.AddData(hash160)
		builder.AddOp(txscript.OP_EQUAL)

		pkScript, err := builder.Script()
		if err != nil {
			return nil, nil, nil, err
		}

		// Generate a valid sigScript that will allow us to spend the
		// p2sh output. The sigScript will contain only a single push of
		// the p2wsh witness program corresponding to the matching
		// public key of this address.
		sigScript, err := txscript.NewScriptBuilder().
			AddData(pks).
			Script()
		if err != nil {
			return nil, nil, nil, err
		}

		address, err := btcutil.NewAddressScriptHash(
			pkScript, chainParams,
		)
		if err != nil {
			return nil, nil, nil, err
		}

		return address, pks, sigScript, nil

	case HtlcP2WSH:
		pkScript, err := input.WitnessScriptHash(script)
		if err != nil {
			return nil, nil, nil, err
		}

		address, err := btcutil.NewAddressWitnessScriptHash(
			pkScript[2:],
			chainParams,
		)
		if err != nil {
			return nil, nil, nil, err
		}

		// Pay to witness script hash (segwit v0) does not need a
		// sigScript (we provide it in the witness instead), so we
		// return nil for our sigScript.
		return address, pkScript, nil, nil

	default:
		return nil, nil, nil, fmt.Errorf("unexpected output type: %v",
			outputType)
	}
}

// GenSuccessWitness returns the success script to spend this htlc with
// the preimage.
func (h *Htlc) GenSuccessWitness(receiverSig []byte,
	preimage lntypes.Preimage) (wire.TxWitness, error) {

	if h.Hash != preimage.Hash() {
		return nil, errors.New("preimage doesn't match hash")
	}

	return h.genSuccessWitness(receiverSig, preimage)
}

// AddSuccessToEstimator adds a successful spend to a weight estimator.
func (h *Htlc) AddSuccessToEstimator(estimator *input.TxWeightEstimator) {
	maxSuccessWitnessSize := h.MaxSuccessWitnessSize()

	switch h.OutputType {
	case HtlcP2WSH:
		estimator.AddWitnessInput(maxSuccessWitnessSize)

	case HtlcNP2WSH:
		estimator.AddNestedP2WSHInput(maxSuccessWitnessSize)

	case HtlcP2TR:
		claimLeaf := txscript.NewBaseTapLeaf(
			h.HtlcScript.SuccessScript(),
		)

		// TODO - find a cleaner way rather than cast to get internal
		// pubkey?
		htlcV3 := h.HtlcScript.(*HtlcScriptV3)

		tapScript := input.TapscriptPartialReveal(
			htlcV3.InternalPubKey, claimLeaf, claimLeaf.TapHash(),
		)

		estimator.AddTapscriptInput(maxSuccessWitnessSize, tapScript)
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

	case HtlcP2TR:
		timeoutLeaf := txscript.NewBaseTapLeaf(
			h.HtlcScript.TimeoutScript(),
		)

		// TODO - find a cleaner way rather than cast to get internal
		// pubkey?
		htlcV3 := h.HtlcScript.(*HtlcScriptV3)

		tapScript := input.TapscriptPartialReveal(
			htlcV3.InternalPubKey, timeoutLeaf,
			timeoutLeaf.TapHash(),
		)

		estimator.AddTapscriptInput(maxTimeoutWitnessSize, tapScript)

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
	preimage lntypes.Preimage) (wire.TxWitness, error) {

	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = append(receiverSig, byte(txscript.SigHashAll))
	witnessStack[1] = preimage[:]
	witnessStack[2] = h.script

	return witnessStack, nil
}

// GenTimeoutWitness returns the timeout script to spend this htlc after
// timeout.
func (h *HtlcScriptV1) GenTimeoutWitness(
	senderSig []byte) (wire.TxWitness, error) {

	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = append(senderSig, byte(txscript.SigHashAll))
	witnessStack[1] = []byte{0}
	witnessStack[2] = h.script

	return witnessStack, nil
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

// TimeoutScript returns the redeem script required to unlock the htlc after
// timeout.
//
// In the case of HtlcScriptV1, this is the full segwit v0 script.
func (h *HtlcScriptV1) TimeoutScript() []byte {
	return h.script
}

// SuccessScript returns the redeem script required to unlock the htlc using
// the preimage.
//
// In the case of HtlcScriptV1, this is the full segwit v0 script.
func (h *HtlcScriptV1) SuccessScript() []byte {
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

// Sighash is the signature hash to use for transactions spending from the htlc.
func (h *HtlcScriptV1) SigHash() txscript.SigHashType {
	return txscript.SigHashAll
}

// lockingConditions return the address, pkScript and sigScript (if
// required) for a htlc script.
func (h *HtlcScriptV1) lockingConditions(htlcOutputType HtlcOutputType,
	params *chaincfg.Params) (btcutil.Address, []byte, []byte, error) {

	return segwitV0LockingConditions(htlcOutputType, params, h.script)
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
	preimage lntypes.Preimage) (wire.TxWitness, error) {

	witnessStack := make(wire.TxWitness, 3)
	witnessStack[0] = preimage[:]
	witnessStack[1] = append(receiverSig, byte(txscript.SigHashAll))
	witnessStack[2] = h.script

	return witnessStack, nil
}

// IsSuccessWitness checks whether the given stack is valid for redeeming the
// htlc.
func (h *HtlcScriptV2) IsSuccessWitness(witness wire.TxWitness) bool {
	isTimeoutTx := len(witness) == 4

	return !isTimeoutTx
}

// GenTimeoutWitness returns the timeout script to spend this htlc after
// timeout.
func (h *HtlcScriptV2) GenTimeoutWitness(
	senderSig []byte) (wire.TxWitness, error) {

	witnessStack := make(wire.TxWitness, 4)
	witnessStack[0] = append(senderSig, byte(txscript.SigHashAll))
	witnessStack[1] = h.senderKey[:]
	witnessStack[2] = []byte{}
	witnessStack[3] = h.script

	return witnessStack, nil
}

// TimeoutScript returns the redeem script required to unlock the htlc after
// timeout.
//
// In the case of HtlcScriptV2, this is the full segwit v0 script.
func (h *HtlcScriptV2) TimeoutScript() []byte {
	return h.script
}

// SuccessScript returns the redeem script required to unlock the htlc using
// the preimage.
//
// In the case of HtlcScriptV2, this is the full segwit v0 script.
func (h *HtlcScriptV2) SuccessScript() []byte {
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

// Sighash is the signature hash to use for transactions spending from the htlc.
func (h *HtlcScriptV2) SigHash() txscript.SigHashType {
	return txscript.SigHashAll
}

// lockingConditions return the address, pkScript and sigScript (if
// required) for a htlc script.
func (h *HtlcScriptV2) lockingConditions(htlcOutputType HtlcOutputType,
	params *chaincfg.Params) (btcutil.Address, []byte, []byte, error) {

	return segwitV0LockingConditions(htlcOutputType, params, h.script)
}

// HtlcScriptV3 encapsulates the htlc v3 script.
type HtlcScriptV3 struct {
	timeoutScript  []byte
	claimScript    []byte
	TaprootKey     *secp.PublicKey
	InternalPubKey *secp.PublicKey
	SenderKey      [33]byte
}

// newHTLCScriptV3 constructs a HtlcScipt with the HTLC V3 taproot script.
func newHTLCScriptV3(cltvExpiry int32, senderHtlcKey,
	receiverHtlcKey [33]byte, sharedKey *btcec.PublicKey,
	swapHash lntypes.Hash) (*HtlcScriptV3, error) {

	// Schnorr keys have implicit sign, remove the sign byte from our
	// compressed key.
	var schnorrSenderKey, schnorrReceiverKey [32]byte
	copy(schnorrSenderKey[:], senderHtlcKey[1:])
	copy(schnorrReceiverKey[:], receiverHtlcKey[1:])

	// Create our claim path script, we'll use this separately
	// later on to generate the claim path leaf.
	claimPathScript, err := GenClaimPathScript(schnorrReceiverKey, swapHash)
	if err != nil {
		return nil, err
	}

	// Create our timeout path leaf, we'll use this separately
	// later on to generate the timeout path leaf.
	timeoutPathScript, err := GenTimeoutPathScript(
		schnorrSenderKey, int64(cltvExpiry),
	)
	if err != nil {
		return nil, err
	}

	// Assemble our taproot script tree from our leaves.
	tree := txscript.AssembleTaprootScriptTree(
		txscript.NewBaseTapLeaf(claimPathScript),
		txscript.NewBaseTapLeaf(timeoutPathScript),
	)

	rootHash := tree.RootNode.TapHash()

	// Calculate top level taproot key, using our shared key as the internal
	// key.
	taprootKey := txscript.ComputeTaprootOutputKey(
		sharedKey, rootHash[:],
	)

	return &HtlcScriptV3{
		timeoutScript:  timeoutPathScript,
		claimScript:    claimPathScript,
		TaprootKey:     taprootKey,
		InternalPubKey: sharedKey,
		SenderKey:      senderHtlcKey,
	}, nil
}

// GenTimeoutPathScript constructs an HtlcScript for the timeout payment path.
//
//	<senderHtlcKey> OP_CHECKSIGVERIFY <cltvExpiry> OP_CHECKLOCKTIMEVERIFY
func GenTimeoutPathScript(
	senderHtlcKey [32]byte, cltvExpiry int64) ([]byte, error) {

	builder := txscript.NewScriptBuilder()
	builder.AddData(senderHtlcKey[:])
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddInt64(cltvExpiry)
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	return builder.Script()
}

// GenClaimPathScript constructs an HtlcScript for the claim payment path.
//
//	<receiverHtlcKey> OP_CHECKSIGVERIFY
//	OP_SIZE 32 OP_EQUALVERIFY
//	OP_HASH160 <ripemd160h(swapHash)> OP_EQUALVERIFY
//	1
//	OP_CHECKSEQUENCEVERIFY
func GenClaimPathScript(
	receiverHtlcKey [32]byte, swapHash lntypes.Hash) ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	builder.AddData(receiverHtlcKey[:])
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(input.Ripemd160H(swapHash[:]))
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddInt64(1)
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	return builder.Script()
}

// genControlBlock constructs the control block with the specified.
func (h *HtlcScriptV3) genControlBlock(leafScript []byte) ([]byte, error) {
	var outputKeyYIsOdd bool

	if h.TaprootKey.SerializeCompressed()[0] == secp.PubKeyFormatCompressedOdd {
		outputKeyYIsOdd = true
	}

	leaf := txscript.NewBaseTapLeaf(leafScript)
	proof := leaf.TapHash()

	controlBlock := txscript.ControlBlock{
		InternalKey:     h.InternalPubKey,
		OutputKeyYIsOdd: outputKeyYIsOdd,
		LeafVersion:     txscript.BaseLeafVersion,
		InclusionProof:  proof[:],
	}
	controlBlockBytes, err := controlBlock.ToBytes()

	if err != nil {
		return nil, err
	}
	return controlBlockBytes, nil
}

// genSuccessWitness returns the success script to spend this htlc with
// the preimage.
func (h *HtlcScriptV3) genSuccessWitness(
	receiverSig []byte, preimage lntypes.Preimage) (wire.TxWitness, error) {

	controlBlockBytes, err := h.genControlBlock(h.timeoutScript)
	if err != nil {
		return nil, err
	}
	return wire.TxWitness{
		preimage[:],
		receiverSig,
		h.claimScript,
		controlBlockBytes,
	}, nil
}

// GenTimeoutWitness returns the timeout script to spend this htlc after
// timeout.
func (h *HtlcScriptV3) GenTimeoutWitness(
	senderSig []byte) (wire.TxWitness, error) {

	controlBlockBytes, err := h.genControlBlock(h.claimScript)
	if err != nil {
		return nil, err
	}
	return wire.TxWitness{
		senderSig,
		h.timeoutScript,
		controlBlockBytes,
	}, nil
}

// IsSuccessWitness checks whether the given stack is valid for
// redeeming the htlc.
func (h *HtlcScriptV3) IsSuccessWitness(witness wire.TxWitness) bool {
	return len(witness) == 4
}

// TimeoutScript returns the redeem script required to unlock the htlc after
// timeout.
//
// In the case of HtlcScriptV3, this is the timeout tapleaf.
func (h *HtlcScriptV3) TimeoutScript() []byte {
	return h.timeoutScript
}

// SuccessScript returns the redeem script required to unlock the htlc using
// the preimage.
//
// In the case of HtlcScriptV3, this is the claim tapleaf.
func (h *HtlcScriptV3) SuccessScript() []byte {
	return h.claimScript
}

// MaxSuccessWitnessSize returns the maximum witness size for the
// success case witness.
func (h *HtlcScriptV3) MaxSuccessWitnessSize() int {
	// Calculate maximum success witness size
	//
	// - number_of_witness_elements: 1 byte
	// - sig_length: 1 byte
	// - sig: 73 bytes
	// - preimage_length: 1 byte
	// - preimage: 32 bytes
	// - witness_script_length: 1 byte
	// - witness_script: len(script) bytes
	// - control_block_length: 1 byte
	// - control_block: 4129 bytes
	return 1 + 1 + 73 + 1 + 32 + 1 + len(h.claimScript) + 1 + 4129
}

// MaxTimeoutWitnessSize returns the maximum witness size for the
// timeout case witness.
func (h *HtlcScriptV3) MaxTimeoutWitnessSize() int {
	// Calculate maximum timeout witness size
	//
	// - number_of_witness_elements: 1 byte
	// - sig_length: 1 byte
	// - sig: 73 bytes
	// - witness_script_length: 1 byte
	// - witness_script: len(script) bytes
	// - control_block_length: 1 byte
	// - control_block: 4129 bytes
	return 1 + 1 + 73 + 1 + len(h.timeoutScript) + 1 + 4129
}

// SuccessSequence returns the sequence to spend this htlc in the
// success case.
func (h *HtlcScriptV3) SuccessSequence() uint32 {
	return 1
}

// Sighash is the signature hash to use for transactions spending from the htlc.
func (h *HtlcScriptV3) SigHash() txscript.SigHashType {
	return txscript.SigHashDefault
}

// lockingConditions return the address, pkScript and sigScript (if required)
// for a htlc script.
func (h *HtlcScriptV3) lockingConditions(outputType HtlcOutputType,
	chainParams *chaincfg.Params) (btcutil.Address, []byte, []byte, error) {

	// HtlcV3 can only have taproot output type, because we utilize
	// tapscript claim paths.
	if outputType != HtlcP2TR {
		return nil, nil, nil, fmt.Errorf("htlc v3 only supports P2TR "+
			"outputs, got: %v", outputType)
	}

	// Generate a tapscript address from our tree.
	address, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(h.TaprootKey),
		chainParams,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	// Generate locking script.
	pkScript, err := txscript.PayToAddrScript(address)
	if err != nil {
		return nil, nil, nil, err
	}

	// Taproot (segwit v1) does not need a sigScript (we provide it in the
	// witness instead), so we return nil for our sigScript.
	return address, pkScript, nil, nil
}
