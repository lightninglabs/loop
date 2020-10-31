package sweep

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
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
	keyBytes [33]byte,
	witnessFunc func(sig []byte) (wire.TxWitness, error),
	amount, destAmount, feeOnlyDest, feeOnlyChange, feeBoth btcutil.Amount,
	destAddr, changeAddr btcutil.Address,
	warnf func(message string, args ...interface{})) (*wire.MsgTx, error) {

	// Compose tx.
	sweepTx := wire.NewMsgTx(2)

	sweepTx.LockTime = uint32(height)

	// Add HTLC input.
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: htlcOutpoint,
		SignatureScript:  htlc.SigScript,
		Sequence:         sequence,
	})

	destinations, err := deduceDestinations(amount, destAmount,
		feeOnlyDest, feeOnlyChange, feeBoth,
		destAddr, changeAddr)
	if err != nil {
		return nil, err
	}
	if len(destinations) == 1 && destinations[0].addr == changeAddr {
		warnf("Not sufficient coin size to send to destAddr, so sending to changeAddr. amount=%s, destination=destAmount, fee=%s. This must be a bug.", amount, destAmount, feeOnlyDest)
	}

	// Add outputs.
	for _, dst := range destinations {
		sweepPkScript, err := txscript.PayToAddrScript(dst.addr)
		if err != nil {
			return nil, err
		}

		sweepTx.AddTxOut(&wire.TxOut{
			PkScript: sweepPkScript,
			Value:    int64(dst.amount),
		})
	}

	// Generate a signature for the swap htlc transaction.

	key, err := btcec.ParsePubKey(keyBytes[:], btcec.S256())
	if err != nil {
		return nil, err
	}

	signDesc := lndclient.SignDescriptor{
		WitnessScript: htlc.Script(),
		Output: &wire.TxOut{
			Value: int64(amount),
		},
		HashType:   txscript.SigHashAll,
		InputIndex: 0,
		KeyDesc: keychain.KeyDescriptor{
			PubKey: key,
		},
	}

	rawSigs, err := s.Lnd.Signer.SignOutputRaw(
		globalCtx, sweepTx, []*lndclient.SignDescriptor{&signDesc},
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
	destAddr, changeAddr btcutil.Address, sweepConfTarget int32) (
	feeOnlyDest, feeOnlyChange, feeBoth btcutil.Amount, err error) {

	// Get fee estimate from lnd.
	feeRate, err := s.Lnd.WalletKit.EstimateFee(ctx, sweepConfTarget)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("estimate fee: %v", err)
	}

	// Calculate feeOnlyDest.
	var estOnlyDest input.TxWeightEstimator
	if err := addOutput(&estOnlyDest, destAddr); err != nil {
		return 0, 0, 0, err
	}
	addInputEstimate(&estOnlyDest)
	feeOnlyDest = feeRate.FeeForWeight(int64(estOnlyDest.Weight()))

	if changeAddr != nil {
		// Calculate feeOnlyChange.
		var estOnlyChange input.TxWeightEstimator
		if err := addOutput(&estOnlyChange, changeAddr); err != nil {
			return 0, 0, 0, err
		}
		addInputEstimate(&estOnlyChange)
		feeOnlyChange = feeRate.FeeForWeight(int64(estOnlyChange.Weight()))

		// Calculate feeBoth.
		var estBoth input.TxWeightEstimator
		if err := addOutput(&estBoth, destAddr); err != nil {
			return 0, 0, 0, err
		}
		if err := addOutput(&estBoth, changeAddr); err != nil {
			return 0, 0, 0, err
		}
		addInputEstimate(&estBoth)
		feeBoth = feeRate.FeeForWeight(int64(estBoth.Weight()))
	}

	return feeOnlyDest, feeOnlyChange, feeBoth, nil
}

func addOutput(weightEstimate *input.TxWeightEstimator, addr btcutil.Address) error {
	switch addr.(type) {
	case *btcutil.AddressWitnessScriptHash:
		weightEstimate.AddP2WSHOutput()
	case *btcutil.AddressWitnessPubKeyHash:
		weightEstimate.AddP2WKHOutput()
	case *btcutil.AddressScriptHash:
		weightEstimate.AddP2SHOutput()
	case *btcutil.AddressPubKeyHash:
		weightEstimate.AddP2PKHOutput()
	default:
		return fmt.Errorf("unknown address type %T", addr)
	}
	return nil
}

const dustOutput = 2020 // FIXME: find the actual value.

type destination struct {
	addr   btcutil.Address
	amount btcutil.Amount
}

func deduceDestinations(amount, destAmount, feeOnlyDest, feeOnlyChange, feeBoth btcutil.Amount,
	destAddr, changeAddr btcutil.Address) ([]destination, error) {

	if (destAmount != 0) != (changeAddr != nil) {
		return nil, fmt.Errorf("provide either both destAmount and changeAddr or none of them")
	}
	if (feeOnlyChange != 0) != (changeAddr != nil) {
		return nil, fmt.Errorf("provide either both feeOnlyChange and changeAddr or none of them")
	}
	if (feeBoth != 0) != (changeAddr != nil) {
		return nil, fmt.Errorf("provide either both feeBoth and changeAddr or none of them")
	}

	if changeAddr == nil {
		// No change. Just put everything on the main destination address.
		return []destination{
			{
				addr:   destAddr,
				amount: amount - feeOnlyDest,
			},
		}, nil
	}

	changeAmount := amount - destAmount - feeBoth

	if changeAmount > dustOutput {
		// Change is large enough. Return tx with 2 outputs.
		return []destination{
			{
				addr:   destAddr,
				amount: destAmount,
			},
			{
				addr:   changeAddr,
				amount: changeAmount,
			},
		}, nil
	}

	// If we are here, then changeAmount is below dustOutput so we drop the change.

	if amount-destAmount >= feeOnlyDest {
		// We still can send destAmount to destAddr.
		return []destination{
			{
				addr:   destAddr,
				amount: destAmount,
			},
		}, nil
	}

	// We can not send destAmount to destAddr. This must be a bug, because we
	// checked in swapClientServer.LoopOut that amt >= max_miner_fee + dest_amt.
	// However it is better to send everything to change address than to crash.
	// The warning is logged by CreateSweepTx.

	return []destination{
		{
			addr:   changeAddr,
			amount: amount - feeOnlyChange,
		},
	}, nil
}
