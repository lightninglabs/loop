package htlc

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

// GenSuccessPathScript constructs the success payment path. The final
// relative lock of one block is part of the version-zero contract and must not
// be changed without introducing a new contract version.
func GenSuccessPathScript(receiverHtlcKey *btcec.PublicKey,
	swapHash lntypes.Hash) ([]byte, error) {

	if receiverHtlcKey == nil {
		return nil, fmt.Errorf("receiver HTLC key is required")
	}

	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(receiverHtlcKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(input.Ripemd160H(swapHash[:]))
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddInt64(int64(SuccessSequence))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	return builder.Script()
}

// GenTimeoutPathScript constructs the timeout payment path.
func GenTimeoutPathScript(senderHtlcKey *btcec.PublicKey, csvExpiry int64) (
	[]byte, error) {

	if senderHtlcKey == nil {
		return nil, fmt.Errorf("sender HTLC key is required")
	}
	if csvExpiry <= 0 {
		return nil, fmt.Errorf("CSV expiry must be positive")
	}
	if csvExpiry > int64(wire.SequenceLockTimeMask) {
		return nil, fmt.Errorf("CSV expiry exceeds block-based BIP68 range")
	}

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

// CreateOpTrueLeaf creates the legacy Taproot Asset script key whose only
// script path is OP_TRUE beneath the public Taproot Assets NUMS key.
func CreateOpTrueLeaf() (asset.ScriptKey, txscript.TapLeaf,
	*txscript.IndexedTapScriptTree, *txscript.ControlBlock, error) {

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
			Tweak: rootHash[:],
		},
	}
	if tapKey.SerializeCompressed()[0] ==
		secp256k1.PubKeyFormatCompressedOdd {

		controlBlock.OutputKeyYIsOdd = true
	}

	return tapScriptKey, tapLeaf, tree, controlBlock, nil
}

// GetOpTrueScriptKey returns the compressed legacy OP_TRUE Taproot Asset
// script key.
func GetOpTrueScriptKey() ([]byte, error) {
	opTrueScriptKey, _, _, _, err := CreateOpTrueLeaf()
	if err != nil {
		return nil, err
	}

	scriptKey, err := schnorr.ParsePubKey(
		opTrueScriptKey.PubKey.SerializeCompressed()[1:],
	)
	if err != nil {
		return nil, err
	}

	return scriptKey.SerializeCompressed(), nil
}
