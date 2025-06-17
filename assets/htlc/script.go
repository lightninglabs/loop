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

// GenSuccessPathScript constructs an HtlcScript for the success payment path.
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
	tapKey := txscript.ComputeTaprootOutputKey(asset.NUMSPubKey, rootHash[:])

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
