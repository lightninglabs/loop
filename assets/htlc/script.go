package htlc

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

// GenSuccessPathScript constructs a script for the success path of the HTLC
// payment. Optionally includes a CHECKSEQUENCEVERIFY (CSV) of 1 if `csv` is
// true, to prevent potential pinning attacks when the HTLC is not part of a
// package relay.
func GenSuccessPathScript(receiverHtlcKey *btcec.PublicKey,
	swapHash lntypes.Hash, csvOne bool) ([]byte, error) {

	builder := txscript.NewScriptBuilder()

	builder.AddData(schnorr.SerializePubKey(receiverHtlcKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(input.Ripemd160H(swapHash[:]))
	// OP_EQUAL will leave 0 or 1 on the stack depending on whether the hash
	// matches.
	// - If it matches and CSV is not used, the script will
	// evaulate to true.
	// - If it matches and CSV is used, we'll have 1 on the stack which is
	// used to verify the CSV condition.
	// - If it does not match, we'll have 0 on the stack which will cause
	// the script to fail even if CSV is used.
	builder.AddOp(txscript.OP_EQUAL)

	if csvOne {
		// If csvOne is true, we add a CHECKSEQUENCEVERIFY to ensure
		// that the HTLC can only be claimed after at least one
		// confirmation.
		builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	}

	return builder.Script()
}

// GenTimeoutPathScript constructs an HtlcScript for the timeout payment path.
func GenTimeoutPathScript(senderHtlcKey *btcec.PublicKey, csvExpiry int64) (
	[]byte, error) {

	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(senderHtlcKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddInt64(csvExpiry)
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	return builder.Script()
}

// GetOpTrueScript returns a script that always evaluates to true.
func GetOpTrueScript() ([]byte, error) {
	return txscript.NewScriptBuilder().AddOp(txscript.OP_TRUE).Script()
}

// CreateOpTrueLeaf creates a taproot leaf that always evaluates to true.
func CreateOpTrueLeaf() (asset.ScriptKey, txscript.TapLeaf,
	*txscript.IndexedTapScriptTree, *txscript.ControlBlock, error) {

	// Create the taproot OP_TRUE script.
	tapScript, err := GetOpTrueScript()
	if err != nil {
		return asset.ScriptKey{}, txscript.TapLeaf{}, nil, nil, err
	}

	tapLeaf := txscript.NewBaseTapLeaf(tapScript)
	tree := txscript.AssembleTaprootScriptTree(tapLeaf)
	rootHash := tree.RootNode.TapHash()
	tapKey := txscript.ComputeTaprootOutputKey(
		asset.NUMSPubKey, rootHash[:],
	)

	merkleRootHash := tree.RootNode.TapHash()

	controlBlock := &txscript.ControlBlock{
		LeafVersion: txscript.BaseLeafVersion,
		InternalKey: asset.NUMSPubKey,
	}
	tapScriptKey := asset.ScriptKey{
		PubKey: tapKey,
		TweakedScriptKey: &asset.TweakedScriptKey{
			RawKey: keychain.KeyDescriptor{
				PubKey: asset.NUMSPubKey,
			},
			Tweak: merkleRootHash[:],
		},
	}
	if tapKey.SerializeCompressed()[0] ==
		secp256k1.PubKeyFormatCompressedOdd {

		controlBlock.OutputKeyYIsOdd = true
	}

	return tapScriptKey, tapLeaf, tree, controlBlock, nil
}
