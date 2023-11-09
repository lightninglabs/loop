package script

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
)

const (
	// TaprootMultiSigWitnessSize evaluates to 66 bytes:
	//	- num_witness_elements: 1 byte
	//	- sig_varint_len: 1 byte
	//	- <sig>: 64 bytes
	TaprootMultiSigWitnessSize = 1 + 1 + 64

	// TaprootExpiryScriptSize evaluates to 39 bytes:
	//	- OP_DATA: 1 byte (client_key length)
	//	- <client_key>: 32 bytes
	//	- OP_CHECKSIGVERIFY: 1 byte
	//	- <address_expiry>: 4 bytes
	//	- OP_CHECKSEQUENCEVERIFY: 1 byte
	TaprootExpiryScriptSize = 1 + 32 + 1 + 4 + 1

	// TaprootExpiryWitnessSize evaluates to 140 bytes:
	//	- num_witness_elements: 1 byte
	//	- client_sig_varint_len: 1 byte (client_sig length)
	//	- <trader_sig>: 64 bytes
	//	- witness_script_varint_len: 1 byte (script length)
	//	- <witness_script>: 39 bytes
	//	- control_block_varint_len: 1 byte (control block length)
	//	- <control_block>: 33 bytes
	TaprootExpiryWitnessSize = 1 + 1 + 64 + 1 + TaprootExpiryScriptSize + 1 + 33
)

// StaticAddress encapsulates the static address script.
type StaticAddress struct {
	// TimeoutScript is the final locking script for the timeout path which
	// is available to the sender after the set block height.
	TimeoutScript []byte

	// TimeoutLeaf is the timeout leaf.
	TimeoutLeaf *txscript.TapLeaf

	// ScriptTree is the assembled script tree from our timeout leaf.
	ScriptTree *txscript.IndexedTapScriptTree

	// InternalPubKey is the public key for the keyspend path which bypasses
	// the timeout script locking.
	InternalPubKey *btcec.PublicKey

	// TaprootKey is the taproot public key which is created with the above
	// 3 inputs.
	TaprootKey *btcec.PublicKey

	// RootHash is the root hash of the taptree.
	RootHash chainhash.Hash
}

// NewStaticAddress constructs a static address script.
func NewStaticAddress(muSig2Version input.MuSig2Version, csvExpiry int64,
	clientPubKey, serverPubKey *btcec.PublicKey) (*StaticAddress, error) {

	// Create our timeout path leaf, we'll use this separately to generate
	// the timeout path leaf.
	timeoutPathScript, err := GenTimeoutPathScript(clientPubKey, csvExpiry)
	if err != nil {
		return nil, err
	}

	// Assemble our taproot script tree from our leaves.
	timeoutLeaf := txscript.NewBaseTapLeaf(timeoutPathScript)
	tree := txscript.AssembleTaprootScriptTree(timeoutLeaf)

	rootHash := tree.RootNode.TapHash()

	// Calculate the internal aggregate key.
	aggregateKey, err := input.MuSig2CombineKeys(
		muSig2Version,
		[]*btcec.PublicKey{clientPubKey, serverPubKey},
		true,
		&input.MuSig2Tweaks{
			TaprootTweak: rootHash[:],
		},
	)
	if err != nil {
		return nil, err
	}

	return &StaticAddress{
		TimeoutScript:  timeoutPathScript,
		TimeoutLeaf:    &timeoutLeaf,
		ScriptTree:     tree,
		InternalPubKey: aggregateKey.PreTweakedKey,
		TaprootKey:     aggregateKey.FinalKey,
		RootHash:       rootHash,
	}, nil
}

// StaticAddressScript creates a MuSig2 2-of-2 multisig script with CSV timeout
// path for the clientKey. This script represents a static loop-in address.
func (s *StaticAddress) StaticAddressScript() ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	builder.AddOp(txscript.OP_1)
	builder.AddData(schnorr.SerializePubKey(s.TaprootKey))

	return builder.Script()
}

// GenTimeoutPathScript constructs a csv timeout script for the client.
//
//	<clientKey> OP_CHECKSIGVERIFY <csvExpiry> OP_CHECKSEQUENCEVERIFY
func GenTimeoutPathScript(clientKey *btcec.PublicKey, csvExpiry int64) ([]byte,
	error) {

	builder := txscript.NewScriptBuilder()

	builder.AddData(schnorr.SerializePubKey(clientKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddInt64(csvExpiry)
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	return builder.Script()
}

// GenSuccessWitness returns the success witness to spend the static address
// output with a combined signature.
func (s *StaticAddress) GenSuccessWitness(combinedSig []byte) (wire.TxWitness,
	error) {

	return wire.TxWitness{
		combinedSig,
	}, nil
}

// GenTimeoutWitness returns the witness to spend the taproot timeout leaf.
func (s *StaticAddress) GenTimeoutWitness(senderSig []byte) (wire.TxWitness,
	error) {

	ctrlBlock := s.ScriptTree.LeafMerkleProofs[0].ToControlBlock(
		s.InternalPubKey,
	)

	ctrlBlockBytes, err := ctrlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return wire.TxWitness{
		senderSig,
		s.TimeoutScript,
		ctrlBlockBytes,
	}, nil
}

// ExpirySpendWeight returns the weight of the expiry path spend.
func ExpirySpendWeight() int64 {
	var weightEstimator input.TxWeightEstimator
	weightEstimator.AddWitnessInput(TaprootExpiryWitnessSize)

	weightEstimator.AddP2TROutput()

	return int64(weightEstimator.Weight())
}
