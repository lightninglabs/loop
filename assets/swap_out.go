package assets

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"

	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

// SwapOut is a struct that represents a swap out. It contains all the
// information needed to perform a swap out.
type SwapOut struct {
	// We embed swapkit for all script related helpers.
	htlc.SwapKit

	// SwapPreimage is the preimage of the swap, that enables spending
	// the success path, it's hash is the main identifier of the swap.
	SwapPreimage lntypes.Preimage

	// State is the current state of the swap.
	State fsm.StateType

	// InitiationHeight is the height at which the swap was initiated.
	InitiationHeight uint32

	// ClientKeyLocator is the key locator of the clients key.
	ClientKeyLocator keychain.KeyLocator

	// HtlcOutPoint is the outpoint of the htlc that was created to
	// perform the swap.
	HtlcOutPoint *wire.OutPoint

	// HtlcConfirmationHeight is the height at which the htlc was
	// confirmed.
	HtlcConfirmationHeight uint32

	// SweepOutpoint is the outpoint of the htlc that was swept.
	SweepOutpoint *wire.OutPoint

	// SweepConfirmationHeight is the height at which the sweep was
	// confirmed.
	SweepConfirmationHeight uint32

	// SweepPkscript is the pkscript of the sweep transaction.
	SweepPkscript []byte

	// RawHtlcProof is the raw htlc proof that we need to send to the
	// receiver. We only keep this in the OutFSM struct as we don't want
	// to save it in the store.
	RawHtlcProof []byte
}

// NewSwapOut creates a new swap out.
func NewSwapOut(swapHash lntypes.Hash, amt uint64,
	assetId []byte, clientKeyDesc *keychain.KeyDescriptor,
	senderPubkey *btcec.PublicKey, csvExpiry, initiationHeight uint32,
) *SwapOut {

	return &SwapOut{
		SwapKit: htlc.SwapKit{
			SwapHash:       swapHash,
			Amount:         amt,
			SenderPubKey:   senderPubkey,
			ReceiverPubKey: clientKeyDesc.PubKey,
			CsvExpiry:      csvExpiry,
			AssetID:        assetId,
		},
		State:            Init,
		InitiationHeight: initiationHeight,
		ClientKeyLocator: clientKeyDesc.KeyLocator,
	}
}

// genTaprootAssetRootFromProof generates the taproot asset root from the proof
// of the swap.
func (s *SwapOut) genTaprootAssetRootFromProof(proof *proof.Proof) ([]byte,
	error) {

	assetCpy := proof.Asset.Copy()
	assetCpy.PrevWitnesses[0].SplitCommitment = nil
	sendCommitment, err := commitment.NewAssetCommitment(
		assetCpy,
	)
	if err != nil {
		return nil, err
	}

	version := commitment.TapCommitmentV2
	assetCommitment, err := commitment.NewTapCommitment(
		&version, sendCommitment,
	)
	if err != nil {
		return nil, err
	}
	taprootAssetRoot := txscript.AssembleTaprootScriptTree(
		assetCommitment.TapLeaf(),
	).RootNode.TapHash()

	return taprootAssetRoot[:], nil
}
