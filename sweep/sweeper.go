package sweep

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
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

	// We need our previous outputs for taproot spends, and there's no
	// harm including them for segwit v0, so we always inlucde our prevOut.
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
	addInputEstimate func(*input.TxWeightEstimator),
	destAddr btcutil.Address, sweepConfTarget int32) (
	btcutil.Amount, error) {

	// Get fee estimate from lnd.
	feeRate, err := s.Lnd.WalletKit.EstimateFee(ctx, sweepConfTarget)
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
	default:
		return 0, fmt.Errorf("unknown address type %T", destAddr)
	}

	addInputEstimate(&weightEstimate)
	weight := weightEstimate.Weight()

	return feeRate.FeeForWeight(int64(weight)), nil
}
