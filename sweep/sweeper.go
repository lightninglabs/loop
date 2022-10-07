package sweep

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// Sweeper creates htlc sweep txes.
type Sweeper struct {
	Lnd *lndclient.LndServices
}

// CreateUnsignedTaprootKeySpendSweepTx creates a taproot htlc sweep tx using
// keyspend. Returns the raw unsigned txn, the psbt serialized txn, the sighash
// or an error.
func (s *Sweeper) CreateUnsignedTaprootKeySpendSweepTx(
	ctx context.Context, lockTime uint32,
	htlc *swap.Htlc, htlcOutpoint wire.OutPoint,
	amount, fee btcutil.Amount, destAddr btcutil.Address) (
	*wire.MsgTx, []byte, []byte, error) {

	if htlc.Version != swap.HtlcV3 {
		return nil, nil, nil, fmt.Errorf("invalid htlc version")
	}

	// Add output for the destination address.
	sweepPkScript, err := txscript.PayToAddrScript(destAddr)
	if err != nil {
		return nil, nil, nil, err
	}

	// Compose tx.
	sweepTx := &wire.MsgTx{
		Version: 2,
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: htlcOutpoint,
		}},
		TxOut: []*wire.TxOut{{
			Value:    int64(amount - fee),
			PkScript: sweepPkScript,
		}},
		LockTime: lockTime,
	}

	packet, err := psbt.NewFromUnsignedTx(sweepTx)
	if err != nil {
		return nil, nil, nil, err
	}

	packet.Inputs[0].WitnessUtxo = &wire.TxOut{
		Value:    int64(amount),
		PkScript: htlc.PkScript,
	}

	var psbtBuf bytes.Buffer
	err = packet.Serialize(&psbtBuf)
	if err != nil {
		return nil, nil, nil, err
	}

	// We now need to create the raw sighash of the transaction, as we'll
	// use it to create the witness for our htlc sweep.
	prevOutputFetcher := txscript.NewCannedPrevOutputFetcher(
		htlc.PkScript, int64(amount),
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevOutputFetcher)

	taprootSigHash, err := txscript.CalcTaprootSignatureHash(
		sigHashes, txscript.SigHashDefault, sweepTx, 0,
		prevOutputFetcher,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	return sweepTx, psbtBuf.Bytes(), taprootSigHash, nil
}

// CreateSweepTx creates an htlc sweep tx.
func (s *Sweeper) CreateSweepTx(
	globalCtx context.Context, height int32, sequence uint32,
	htlc *swap.Htlc, htlcOutpoint wire.OutPoint,
	keyBytes [33]byte, witnessScript []byte,
	witnessFunc func(sig []byte) (wire.TxWitness, error),
	amount, fee btcutil.Amount,
	destAddr btcutil.Address) (*wire.MsgTx, error) {

	// Compose tx.
	sweepTx := wire.NewMsgTx(2)

	sweepTx.LockTime = uint32(height)

	// Add HTLC input.
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: htlcOutpoint,
		SignatureScript:  htlc.SigScript,
		Sequence:         sequence,
	})

	// Add output for the destination address.
	sweepPkScript, err := txscript.PayToAddrScript(destAddr)
	if err != nil {
		return nil, err
	}

	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: sweepPkScript,
		Value:    int64(amount - fee),
	})

	// Generate a signature for the swap htlc transaction.

	key, err := btcec.ParsePubKey(keyBytes[:])
	if err != nil {
		return nil, err
	}

	signDesc := lndclient.SignDescriptor{
		WitnessScript: witnessScript,
		Output: &wire.TxOut{
			Value:    int64(amount),
			PkScript: htlc.PkScript,
		},
		HashType:   htlc.SigHash(),
		InputIndex: 0,
		KeyDesc: keychain.KeyDescriptor{
			PubKey: key,
		},
	}

	// Update the sign method from the default witness_v0 if this is a
	// taproot htlc. Note that we'll always be doing script spend when
	// sweeping a taproot htlc using the CreateSweepTx function.
	if htlc.Version == swap.HtlcV3 {
		signDesc.SignMethod = input.TaprootScriptSpendSignMethod
	}

	// We need our previous outputs for taproot spends, and there's no
	// harm including them for segwit v0, so we always include our prevOut.
	prevOut := []*wire.TxOut{
		{
			Value:    int64(amount),
			PkScript: htlc.PkScript,
		},
	}

	rawSigs, err := s.Lnd.Signer.SignOutputRaw(
		globalCtx, sweepTx, []*lndclient.SignDescriptor{&signDesc},
		prevOut,
	)
	if err != nil {
		return nil, fmt.Errorf("signing: %v", err)
	}
	sig := rawSigs[0]

	// Add witness stack to the tx input.
	sweepTx.TxIn[0].Witness, err = witnessFunc(sig)
	if err != nil {
		return nil, err
	}

	return sweepTx, nil
}

// GetSweepFee calculates the required tx fee to spend to P2WKH. It takes a
// function that is expected to add the weight of the input to the weight
// estimator.
func (s *Sweeper) GetSweepFee(ctx context.Context,
	addInputEstimate func(*input.TxWeightEstimator) error,
	destAddr btcutil.Address, sweepConfTarget int32) (
	btcutil.Amount, error) {

	// Get fee estimate from lnd.
	feeRate, err := s.Lnd.WalletKit.EstimateFeeRate(ctx, sweepConfTarget)
	if err != nil {
		return 0, fmt.Errorf("estimate fee: %v", err)
	}

	// Calculate weight for this tx.
	var weightEstimate input.TxWeightEstimator
	switch destAddr.(type) {
	case *btcutil.AddressWitnessScriptHash:
		weightEstimate.AddP2WSHOutput()

	case *btcutil.AddressWitnessPubKeyHash:
		weightEstimate.AddP2WKHOutput()

	case *btcutil.AddressScriptHash:
		weightEstimate.AddP2SHOutput()

	case *btcutil.AddressPubKeyHash:
		weightEstimate.AddP2PKHOutput()

	case *btcutil.AddressTaproot:
		weightEstimate.AddP2TROutput()

	default:
		return 0, fmt.Errorf("estimate fee: unknown address type %T",
			destAddr)
	}

	err = addInputEstimate(&weightEstimate)
	if err != nil {
		return 0, err
	}

	weight := weightEstimate.Weight()

	return feeRate.FeeForWeight(int64(weight)), nil
}
