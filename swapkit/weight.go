package swapkit

import (
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
)

// MaxSuccessWitnessSize returns the serialized size of a success witness with
// a 32-byte preimage and the largest accepted Schnorr signature, including its
// optional sighash byte. It uses the actual leaf and control block, including
// the asset commitment sibling. Witnesses produced by the kit have no annex.
func (s *SwapAnchorSpendInfo) MaxSuccessWitnessSize() lntypes.WeightUnit {
	var signature [schnorr.SignatureSize + 1]byte
	var preimage [32]byte
	return lntypes.WeightUnit(wire.TxWitness{
		preimage[:], signature[:], s.LeafScript, s.ControlBlock,
	}.SerializeSize())
}

// MaxTimeoutWitnessSize returns the serialized size of a refund witness with
// the largest accepted Schnorr signature, including its optional sighash byte.
// It uses the actual leaf and asset-aware control block and includes no annex.
func (s *SwapAnchorSpendInfo) MaxTimeoutWitnessSize() lntypes.WeightUnit {
	var signature [schnorr.SignatureSize + 1]byte
	return lntypes.WeightUnit(wire.TxWitness{
		signature[:], s.LeafScript, s.ControlBlock,
	}.SerializeSize())
}

// AddSuccessToEstimator adds the complete success input to a weight estimate.
// Its leaf and asset commitment proof are already included in the witness size.
func (s *SwapAnchorSpendInfo) AddSuccessToEstimator(
	estimator *input.TxWeightEstimator) {

	estimator.AddWitnessInput(s.MaxSuccessWitnessSize())
}

// AddTimeoutToEstimator adds the complete refund input to a weight estimate.
// Its leaf and asset commitment proof are already included in the witness size.
func (s *SwapAnchorSpendInfo) AddTimeoutToEstimator(
	estimator *input.TxWeightEstimator) {

	estimator.AddWitnessInput(s.MaxTimeoutWitnessSize())
}
