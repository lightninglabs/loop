package swap

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
)

// HtlcOutputType defines the output type of the htlc that is published.
type HtlcOutputType uint8

const (
	// HtlcP2WSH is a pay-to-witness-script-hash output (segwit only).
	HtlcP2WSH HtlcOutputType = iota

	// HtlcP2TR is a pay-to-taproot output with three separate spend paths.
	HtlcP2TR
)

// ScriptVersion defines the HTLC script version.
type ScriptVersion uint8

const (
	// HtlcV2 refers to the improved version of the HTLC script.
	HtlcV2 ScriptVersion = iota

	// HtlcV3 refers to an upgraded version of HtlcV2 implemented with
	// tapscript.
	HtlcV3
)

// htlcScript defines an interface for the different HTLC implementations.
type HtlcScript interface {
	// genSuccessWitness returns the success script to spend this htlc with
	// the preimage.
	genSuccessWitness(receiverSig []byte,
		preimage lntypes.Preimage) (wire.TxWitness, error)

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
	// dummyPubKey is a valid public key use for the quote htlc
	// construction.
	dummyPubKey = [33]byte{
		0x03, 0x26, 0x89, 0xc7, 0xc2, 0xda, 0xb1, 0x33, 0x09, 0xfb,
		0x14, 0x3e, 0x0e, 0x8f, 0xe3, 0x96, 0x34, 0x25, 0x21, 0x88,
		0x7e, 0x97, 0x66, 0x90, 0xb6, 0xb4, 0x7f, 0x5b, 0x2a, 0x4b,
		0x7d, 0x44, 0x8e,
	}

	// quoteHash is an empty hash used for the quote htlc construction.
	quoteHash lntypes.Hash

	// QuoteHtlcP2WSH is a template script just used for sweep fee
	// estimation.
	QuoteHtlcP2WSH, _ = NewHtlcV2(
		^int32(0), dummyPubKey, dummyPubKey, quoteHash,
		&chaincfg.MainNetParams,
	)

	// QuoteHtlcP2TR is a template script just used for sweep fee
	// estimation.
	QuoteHtlcP2TR, _ = NewHtlcV3(
		input.MuSig2Version100RC2, ^int32(0), dummyPubKey, dummyPubKey,
		dummyPubKey, dummyPubKey, quoteHash, &chaincfg.MainNetParams,
	)

	// ErrInvalidScriptVersion is returned when an unknown htlc version
	// is provided to NewHtlc. The supported version are HtlcV2, HtlcV3
	// as enums.
	ErrInvalidScriptVersion = fmt.Errorf("invalid script version")

	// ErrInvalidOutputSelected is returned when a taproot output is
	// selected for a v2 script.
	ErrInvalidOutputSelected = fmt.Errorf("taproot output selected for " +
		"non taproot htlc")

	// ErrInvalidOutputType is returned when an unknown output type is
	// associated with a certain swap htlc.
	ErrInvalidOutputType = fmt.Errorf("invalid htlc output type")
)

// String returns the string value of HtlcOutputType.
func (h HtlcOutputType) String() string {
	switch h {
	case HtlcP2WSH:
		return "P2WSH"

	case HtlcP2TR:
		return "P2TR"

	default:
		return "unknown"
	}
}

// NewHtlcV2 returns a new V2 (P2WSH) HTLC instance.
func NewHtlcV2(cltvExpiry int32, senderKey, receiverKey [33]byte,
	hash lntypes.Hash, chainParams *chaincfg.Params) (*Htlc, error) {

	htlc, err := newHTLCScriptV2(
		cltvExpiry, senderKey, receiverKey, hash,
	)

	if err != nil {
		return nil, err
	}

	address, pkScript, sigScript, err := htlc.lockingConditions(
		HtlcP2WSH, chainParams,
	)
	if err != nil {
		return nil, fmt.Errorf("could not get address: %w", err)
	}

	return &Htlc{
		HtlcScript:  htlc,
		Hash:        hash,
		Version:     HtlcV2,
		PkScript:    pkScript,
		OutputType:  HtlcP2WSH,
		ChainParams: chainParams,
		Address:     address,
		SigScript:   sigScript,
	}, nil
}

// NewHtlcV3 returns a new V3 HTLC (P2TR) instance. Internal pubkey generated
// by both participants must be provided.
func NewHtlcV3(muSig2Version input.MuSig2Version, cltvExpiry int32,
	senderInternalKey, receiverInternalKey, senderKey, receiverKey [33]byte,
	hash lntypes.Hash, chainParams *chaincfg.Params) (*Htlc, error) {

	htlc, err := newHTLCScriptV3(
		muSig2Version, cltvExpiry, senderInternalKey,
		receiverInternalKey, senderKey, receiverKey, hash,
	)

	if err != nil {
		return nil, err
	}

	address, pkScript, sigScript, err := htlc.lockingConditions(
		HtlcP2TR, chainParams,
	)
	if err != nil {
		return nil, fmt.Errorf("could not get address: %w", err)
	}

	return &Htlc{
		HtlcScript:  htlc,
		Hash:        hash,
		Version:     HtlcV3,
		PkScript:    pkScript,
		OutputType:  HtlcP2TR,
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
		return nil, nil, nil, fmt.Errorf("unexpected output type: %d",
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
func (h *Htlc) AddSuccessToEstimator(estimator *input.TxWeightEstimator) error {
	maxSuccessWitnessSize := h.MaxSuccessWitnessSize()

	switch h.OutputType {
	case HtlcP2TR:
		// Generate tapscript.
		trHtlc, ok := h.HtlcScript.(*HtlcScriptV3)
		if !ok {
			return ErrInvalidOutputSelected
		}
		successLeaf := txscript.NewBaseTapLeaf(trHtlc.SuccessScript())
		timeoutLeaf := txscript.NewBaseTapLeaf(trHtlc.TimeoutScript())
		timeoutLeafHash := timeoutLeaf.TapHash()

		tapscript := input.TapscriptPartialReveal(
			trHtlc.InternalPubKey, successLeaf, timeoutLeafHash[:],
		)

		estimator.AddTapscriptInput(maxSuccessWitnessSize, tapscript)

	case HtlcP2WSH:
		estimator.AddWitnessInput(maxSuccessWitnessSize)
	}

	return nil
}

// AddTimeoutToEstimator adds a timeout spend to a weight estimator.
func (h *Htlc) AddTimeoutToEstimator(estimator *input.TxWeightEstimator) error {
	maxTimeoutWitnessSize := h.MaxTimeoutWitnessSize()

	switch h.OutputType {
	case HtlcP2TR:
		// Generate tapscript.
		trHtlc, ok := h.HtlcScript.(*HtlcScriptV3)
		if !ok {
			return ErrInvalidOutputSelected
		}
		successLeaf := txscript.NewBaseTapLeaf(trHtlc.SuccessScript())
		timeoutLeaf := txscript.NewBaseTapLeaf(trHtlc.TimeoutScript())
		successLeafHash := successLeaf.TapHash()

		tapscript := input.TapscriptPartialReveal(
			trHtlc.InternalPubKey, timeoutLeaf, successLeafHash[:],
		)

		estimator.AddTapscriptInput(maxTimeoutWitnessSize, tapscript)

	case HtlcP2WSH:
		estimator.AddWitnessInput(maxTimeoutWitnessSize)
	}

	return nil
}

// HtlcScriptV2 encapsulates the htlc v2 script.
type HtlcScriptV2 struct {
	script    []byte
	senderKey [33]byte
}

// newHTLCScriptV2 construct an HtlcScipt with the HTLC V2 witness script.
//
// <receiverHtlcKey> OP_CHECKSIG OP_NOTIF
//
//	OP_DUP OP_HASH160 <HASH160(senderHtlcKey)> OP_EQUALVERIFY OP_CHECKSIGVERIFY
//	<cltv timeout> OP_CHECKLOCKTIMEVERIFY
//
// OP_ELSE
//
//	OP_SIZE <20> OP_EQUALVERIFY OP_HASH160 <ripemd(swapHash)> OP_EQUALVERIFY 1
//	OP_CHECKSEQUENCEVERIFY
//
// OP_ENDIF .
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
	// timeoutScript is the final locking script for the timeout path which
	// is available to the sender after the set blockheight.
	timeoutScript []byte

	// successScript is the final locking script for the success path in
	// which the receiver reveals the preimage.
	successScript []byte

	// InternalPubKey is the public key for the keyspend path which bypasses
	// the above two locking scripts.
	InternalPubKey *btcec.PublicKey

	// TaprootKey is the taproot public key which is created with the above
	// 3 inputs.
	TaprootKey *btcec.PublicKey

	// RootHash is the root hash of the taptree.
	RootHash chainhash.Hash
}

// parsePubKey will parse a serialized public key into a btcec.PublicKey
// depending on the passed MuSig2 version.
func parsePubKey(muSig2Version input.MuSig2Version, key [33]byte) (
	*btcec.PublicKey, error) {

	// Make sure that we have the correct public keys depending on the
	// MuSig2 version.
	switch muSig2Version {
	case input.MuSig2Version100RC2:
		return btcec.ParsePubKey(key[:])

	case input.MuSig2Version040:
		return schnorr.ParsePubKey(key[1:])

	default:
		return nil, fmt.Errorf("unsupported MuSig2 version: %v",
			muSig2Version)
	}
}

// newHTLCScriptV3 constructs a HtlcScipt with the HTLC V3 taproot script.
func newHTLCScriptV3(muSig2Version input.MuSig2Version, cltvExpiry int32,
	senderInternalKey, receiverInternalKey, senderHtlcKey,
	receiverHtlcKey [33]byte, swapHash lntypes.Hash) (*HtlcScriptV3, error) {

	senderPubKey, err := parsePubKey(muSig2Version, senderHtlcKey)
	if err != nil {
		return nil, err
	}

	receiverPubKey, err := parsePubKey(muSig2Version, receiverHtlcKey)
	if err != nil {
		return nil, err
	}

	// Create our success path script, we'll use this separately
	// to generate the success path leaf.
	successPathScript, err := GenSuccessPathScript(
		receiverPubKey, swapHash,
	)
	if err != nil {
		return nil, err
	}

	// Create our timeout path leaf, we'll use this separately
	// to generate the timeout path leaf.
	timeoutPathScript, err := GenTimeoutPathScript(
		senderPubKey, int64(cltvExpiry),
	)
	if err != nil {
		return nil, err
	}

	// Assemble our taproot script tree from our leaves.
	tree := txscript.AssembleTaprootScriptTree(
		txscript.NewBaseTapLeaf(successPathScript),
		txscript.NewBaseTapLeaf(timeoutPathScript),
	)

	rootHash := tree.RootNode.TapHash()

	// Parse the pub keys used in the internal aggregate key. They are
	// optional and may just be the same keys that are used for the script
	// paths.
	senderInternalPubKey, err := parsePubKey(
		muSig2Version, senderInternalKey,
	)
	if err != nil {
		return nil, err
	}

	receiverInternalPubKey, err := parsePubKey(
		muSig2Version, receiverInternalKey,
	)
	if err != nil {
		return nil, err
	}

	var aggregateKey *musig2.AggregateKey
	// Calculate the internal aggregate key.
	aggregateKey, err = input.MuSig2CombineKeys(
		muSig2Version,
		[]*btcec.PublicKey{
			senderInternalPubKey, receiverInternalPubKey,
		},
		true,
		&input.MuSig2Tweaks{
			TaprootTweak: rootHash[:],
		},
	)
	if err != nil {
		return nil, err
	}

	return &HtlcScriptV3{
		timeoutScript:  timeoutPathScript,
		successScript:  successPathScript,
		InternalPubKey: aggregateKey.PreTweakedKey,
		TaprootKey:     aggregateKey.FinalKey,
		RootHash:       rootHash,
	}, nil
}

// GenTimeoutPathScript constructs an HtlcScript for the timeout payment path.
// Largest possible bytesize of the script is 32 + 1 + 2 + 1 = 36.
//
//	<senderHtlcKey> OP_CHECKSIGVERIFY <cltvExpiry> OP_CHECKLOCKTIMEVERIFY
func GenTimeoutPathScript(senderHtlcKey *btcec.PublicKey, cltvExpiry int64) (
	[]byte, error) {

	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(senderHtlcKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddInt64(cltvExpiry)
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	return builder.Script()
}

// GenSuccessPathScript constructs an HtlcScript for the success payment path.
// Largest possible bytesize of the script is 32 + 5*1 + 20 + 3*1 = 60.
//
//	<receiverHtlcKey> OP_CHECKSIGVERIFY
//	OP_SIZE 32 OP_EQUALVERIFY
//	OP_HASH160 <ripemd160h(swapHash)> OP_EQUALVERIFY
//	1 OP_CHECKSEQUENCEVERIFY
func GenSuccessPathScript(receiverHtlcKey *btcec.PublicKey,
	swapHash lntypes.Hash) ([]byte, error) {

	builder := txscript.NewScriptBuilder()

	builder.AddData(schnorr.SerializePubKey(receiverHtlcKey))
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

// genControlBlock constructs the control block with the depth 1 leaf of the
// unused path to compute the proof. For example if spending path a of (root ->
// a, root -> b), genControlBlock(b.Script) would be used to create the
// controlBlock for a.
func (h *HtlcScriptV3) genControlBlock(leafScript []byte) ([]byte, error) {
	var outputKeyYIsOdd bool

	// Check for odd bit.
	if h.TaprootKey.SerializeCompressed()[0] == secp.PubKeyFormatCompressedOdd {
		outputKeyYIsOdd = true
	}

	// Generate proof with unused script path.
	leaf := txscript.NewBaseTapLeaf(leafScript)
	proof := leaf.TapHash()

	controlBlock := txscript.ControlBlock{
		InternalKey:     h.InternalPubKey,
		OutputKeyYIsOdd: outputKeyYIsOdd,
		LeafVersion:     txscript.BaseLeafVersion,
		InclusionProof:  proof[:],
	}

	return controlBlock.ToBytes()
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
		h.successScript,
		controlBlockBytes,
	}, nil
}

// GenTimeoutWitness returns the timeout script to spend this htlc after
// timeout.
func (h *HtlcScriptV3) GenTimeoutWitness(
	senderSig []byte) (wire.TxWitness, error) {

	controlBlockBytes, err := h.genControlBlock(h.successScript)
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
	// The witness has four elements if this is a script spend or one
	// element if this is a keyspend.
	return len(witness) == 4 || len(witness) == 1
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
	return h.successScript
}

// MaxSuccessWitnessSize returns the maximum witness size for the
// success case witness.
func (h *HtlcScriptV3) MaxSuccessWitnessSize() int {
	// Calculate maximum success witness size
	//
	// - number_of_witness_elements: 1 byte
	// - sigLength: 1 byte
	// - sig: 64 bytes
	// - preimage_length: 1 byte
	// - preimage: 32 bytes
	// - witness_script_length: 1 byte
	// - witness_script: 60 bytes
	// - control_block_length: 1 byte
	// - control_block: 65 bytes
	//	- leafVersionAndParity: 1
	//	- internalPubkey: 32
	//	- proof: 32
	return 1 + 1 + 64 + 1 + 32 + 1 + 60 + 1 + 65
}

// MaxTimeoutWitnessSize returns the maximum witness size for the
// timeout case witness.
func (h *HtlcScriptV3) MaxTimeoutWitnessSize() int {
	// Calculate maximum timeout witness size
	//
	// - number_of_witness_elements: 1 byte
	// - sigLength: 1 byte
	// - sig: 64 bytes
	// - witness_script_length: 1 byte
	// - witness_script: 36 bytes
	// - control_block_length: 1 byte
	// - control_block: 65 bytes
	//	- leafVersionAndParity: 1
	//	- internalPubkey: 32
	//	- proof: 32
	return 1 + 1 + 64 + 1 + 36 + 1 + 65
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
		schnorr.SerializePubKey(h.TaprootKey), chainParams,
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
