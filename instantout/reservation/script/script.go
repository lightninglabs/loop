package script

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
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

// ReservationScript returns the tapscript pkscript for the given reservation
// parameters.
func ReservationScript(expiry uint32, serverKey,
	clientKey *btcec.PublicKey) ([]byte, error) {

	aggregatedKey, err := TaprootKey(expiry, serverKey, clientKey)
	if err != nil {
		return nil, err
	}

	return PayToWitnessTaprootScript(aggregatedKey.FinalKey)
}

// TaprootKey returns the aggregated MuSig2 combined key.
func TaprootKey(expiry uint32, serverKey,
	clientKey *btcec.PublicKey) (*musig2.AggregateKey, error) {

	expiryLeaf, err := TaprootExpiryScript(expiry, serverKey)
	if err != nil {
		return nil, err
	}

	rootHash := expiryLeaf.TapHash()

	aggregateKey, err := input.MuSig2CombineKeys(
		input.MuSig2Version100RC2,
		[]*btcec.PublicKey{
			clientKey, serverKey,
		}, true,
		&input.MuSig2Tweaks{
			TaprootTweak: rootHash[:],
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error combining keys: %v", err)
	}

	return aggregateKey, nil
}

// PayToWitnessTaprootScript creates a new script to pay to a version 1
// (taproot) witness program.
func PayToWitnessTaprootScript(taprootKey *btcec.PublicKey) ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	builder.AddOp(txscript.OP_1)
	builder.AddData(schnorr.SerializePubKey(taprootKey))

	return builder.Script()
}

// TaprootExpiryScript returns the leaf script of the expiry script path.
//
// <server_key> OP_CHECKSIGVERIFY <reservation_expiry> OP_CHECKLOCKTIMEVERIFY.
func TaprootExpiryScript(expiry uint32,
	serverKey *btcec.PublicKey) (*txscript.TapLeaf, error) {

	builder := txscript.NewScriptBuilder()

	builder.AddData(schnorr.SerializePubKey(serverKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)

	builder.AddInt64(int64(expiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)

	script, err := builder.Script()
	if err != nil {
		return nil, err
	}

	leaf := txscript.NewBaseTapLeaf(script)
	return &leaf, nil
}

// ExpirySpendWeight returns the weight of the expiry path spend.
func ExpirySpendWeight() int64 {
	var weightEstimator input.TxWeightEstimator
	weightEstimator.AddWitnessInput(TaprootExpiryWitnessSize)

	weightEstimator.AddP2TROutput()

	return int64(weightEstimator.Weight())
}
