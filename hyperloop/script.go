package hyperloop

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/input"
)

const (

	// TaprootMultiSigWitnessSize evaluates to 66 bytes:
	//	- num_witness_elements: 1 byte
	//	- sig_varint_len: 1 byte
	//	- <sig>: 64 bytes
	TaprootMultiSigWitnessSize = 1 + 1 + 64

	// TaprootExpiryScriptSize evaluates to 39 bytes:
	//	- OP_DATA: 1 byte (trader_key length)
	//	- <trader_key>: 32 bytes
	//	- OP_CHECKSIGVERIFY: 1 byte
	//	- <reservation_expiry>: 4 bytes
	//	- OP_CHECKLOCKTIMEVERIFY: 1 byte
	TaprootExpiryScriptSize = 1 + 32 + 1 + 4 + 1

	// TaprootExpiryWitnessSize evaluates to 140 bytes:
	//	- num_witness_elements: 1 byte
	//	- trader_sig_varint_len: 1 byte (trader_sig length)
	//	- <trader_sig>: 64 bytes
	//	- witness_script_varint_len: 1 byte (script length)
	//	- <witness_script>: 39 bytes
	//	- control_block_varint_len: 1 byte (control block length)
	//	- <control_block>: 33 bytes
	TaprootExpiryWitnessSize = 1 + 1 + 64 + 1 + TaprootExpiryScriptSize + 1 + 33
)

func HyperLoopScript(expiry uint32, serverKey *btcec.PublicKey,
	clientKeys []*btcec.PublicKey) ([]byte, error) {

	taprootKey, _, _, err := TaprootKey(expiry, serverKey, clientKeys)
	if err != nil {
		return nil, err
	}
	return PayToWitnessTaprootScript(taprootKey)
}

// TaprootKey returns the aggregated MuSig2 combined internal key and the
// tweaked Taproot key of an hyperloop output, as well as the expiry script tap
// leaf.
func TaprootKey(expiry uint32, serverKey *btcec.PublicKey,
	clientKeys []*btcec.PublicKey) (*btcec.PublicKey, *btcec.PublicKey,
	*txscript.TapLeaf, error) {

	expiryLeaf, err := TaprootExpiryScript(expiry, serverKey)
	if err != nil {
		return nil, nil, nil, err
	}

	rootHash := expiryLeaf.TapHash()

	aggregateKey, err := input.MuSig2CombineKeys(
		input.MuSig2Version100RC2,
		clientKeys,
		true,
		&input.MuSig2Tweaks{
			TaprootTweak: rootHash[:],
		},
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error combining keys: %v", err)
	}

	return aggregateKey.FinalKey, aggregateKey.PreTweakedKey, expiryLeaf, nil
}

// PayToWitnessTaprootScript creates a new script to pay to a version 1
// (taproot) witness program. The passed hash is expected to be valid.
func PayToWitnessTaprootScript(taprootKey *btcec.PublicKey) ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	builder.AddOp(txscript.OP_1)
	builder.AddData(schnorr.SerializePubKey(taprootKey))

	return builder.Script()
}

// TaprootExpiryScript returns the leaf script of the expiry script path.
//
// <server_key> OP_CHECKSIGVERIFY <hyperloop_expiry> OP_CHECKSEQUENCEVERIFY.
func TaprootExpiryScript(expiry uint32,
	serverKey *btcec.PublicKey) (*txscript.TapLeaf, error) {

	builder := txscript.NewScriptBuilder()

	builder.AddData(schnorr.SerializePubKey(serverKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)

	builder.AddInt64(int64(expiry))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	script, err := builder.Script()
	if err != nil {
		return nil, err
	}

	leaf := txscript.NewBaseTapLeaf(script)
	return &leaf, nil
}

func ExpirySpendWeight() int64 {
	var weightEstimator input.TxWeightEstimator
	weightEstimator.AddWitnessInput(TaprootExpiryWitnessSize)

	weightEstimator.AddP2TROutput()

	return int64(weightEstimator.Weight())
}
