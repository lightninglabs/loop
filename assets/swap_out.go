package assets

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapsend"

	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

// SwapOut is a struct that represents a swap out. It contains all the
// information needed to perform a swap out.
type SwapOut struct {
	// SwapHash is the hash of the swap.
	SwapHash lntypes.Hash

	// SwapPreimage is the preimage of the swap, that enables spending
	// the success path, it's hash is the main identifier of the swap.
	SwapPreimage lntypes.Preimage

	// State is the current state of the swap.
	State fsm.StateType

	// Amount is the amount of the asset to swap.
	Amount btcutil.Amount

	// SenderPubkey is the pubkey of the sender of onchain asset funds.
	SenderPubkey *btcec.PublicKey

	// ReceiverPubkey is the pubkey of the receiver of the onchain asset
	// funds.
	ReceiverPubkey *btcec.PublicKey

	// CsvExpiry is the relative timelock in blocks for the swap.
	CsvExpiry int32

	// AssetId is the identifier of the asset to swap.
	AssetId []byte

	// InitiationHeight is the height at which the swap was initiated.
	InitiationHeight int32

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
func NewSwapOut(swapHash lntypes.Hash, amt btcutil.Amount,
	assetId []byte, clientKeyDesc *keychain.KeyDescriptor,
	senderPubkey *btcec.PublicKey, csvExpiry, initiationHeight int32,
) *SwapOut {

	return &SwapOut{
		SwapHash:         swapHash,
		State:            Init,
		Amount:           amt,
		SenderPubkey:     senderPubkey,
		ReceiverPubkey:   clientKeyDesc.PubKey,
		CsvExpiry:        csvExpiry,
		InitiationHeight: initiationHeight,
		ClientKeyLocator: clientKeyDesc.KeyLocator,
		AssetId:          assetId,
	}
}

// GetSuccesScript returns the success path script of the swap.
func (s *SwapOut) GetSuccesScript() ([]byte, error) {
	return GenSuccessPathScript(s.ReceiverPubkey, s.SwapHash)
}

// GetTimeoutScript returns the timeout path script of the swap.
func (s *SwapOut) GetTimeoutScript() ([]byte, error) {
	return GenTimeoutPathScript(s.SenderPubkey, int64(s.CsvExpiry))
}

// getAggregateKey returns the aggregate musig2 key of the swap.
func (s *SwapOut) getAggregateKey() (*btcec.PublicKey, error) {
	aggregateKey, err := input.MuSig2CombineKeys(
		input.MuSig2Version100RC2,
		[]*btcec.PublicKey{
			s.SenderPubkey, s.ReceiverPubkey,
		},
		true,
		&input.MuSig2Tweaks{},
	)
	if err != nil {
		return nil, err
	}

	return aggregateKey.PreTweakedKey, nil
}

// GetTimeOutLeaf returns the timeout leaf of the swap.
func (s *SwapOut) GetTimeOutLeaf() (txscript.TapLeaf, error) {
	timeoutScript, err := s.GetTimeoutScript()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	timeoutLeaf := txscript.NewBaseTapLeaf(timeoutScript)

	return timeoutLeaf, nil
}

// GetSuccessLeaf returns the success leaf of the swap.
func (s *SwapOut) GetSuccessLeaf() (txscript.TapLeaf, error) {
	successScript, err := s.GetSuccesScript()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	successLeaf := txscript.NewBaseTapLeaf(successScript)

	return successLeaf, nil
}

// getSiblingPreimage returns the sibling preimage of the htlc bitcoin toplevel
// output.
func (s *SwapOut) getSiblingPreimage() (commitment.TapscriptPreimage, error) {
	timeOutLeaf, err := s.GetTimeOutLeaf()
	if err != nil {
		return commitment.TapscriptPreimage{}, err
	}

	successLeaf, err := s.GetSuccessLeaf()
	if err != nil {
		return commitment.TapscriptPreimage{}, err
	}

	branch := txscript.NewTapBranch(timeOutLeaf, successLeaf)

	siblingPreimage := commitment.NewPreimageFromBranch(branch)

	return siblingPreimage, nil
}

// createSweepVpkt creates the vpacket for the sweep.
func (s *SwapOut) createSweepVpkt(ctx context.Context, htlcProof *proof.Proof,
	scriptKey asset.ScriptKey, internalKey keychain.KeyDescriptor,
) (*tappsbt.VPacket, error) {

	sweepVpkt, err := tappsbt.FromProofs(
		[]*proof.Proof{htlcProof}, &address.RegressionNetTap,
	)
	if err != nil {
		return nil, err
	}
	sweepVpkt.Outputs = append(sweepVpkt.Outputs, &tappsbt.VOutput{
		AssetVersion:            asset.Version(1),
		Amount:                  uint64(s.Amount),
		Interactive:             true,
		AnchorOutputIndex:       0,
		ScriptKey:               scriptKey,
		AnchorOutputInternalKey: internalKey.PubKey,
	})
	sweepVpkt.Outputs[0].SetAnchorInternalKey(
		internalKey, address.RegressionNetTap.HDCoinType,
	)

	err = tapsend.PrepareOutputAssets(ctx, sweepVpkt)
	if err != nil {
		return nil, err
	}

	_, _, _, controlBlock, err := createOpTrueLeaf()
	if err != nil {
		return nil, err
	}

	controlBlockBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	opTrueScript, err := GetOpTrueScript()
	if err != nil {
		return nil, err
	}

	witness := wire.TxWitness{
		opTrueScript,
		controlBlockBytes,
	}
	firstPrevWitness := &sweepVpkt.Outputs[0].Asset.PrevWitnesses[0]
	if sweepVpkt.Outputs[0].Asset.HasSplitCommitmentWitness() {
		rootAsset := firstPrevWitness.SplitCommitment.RootAsset
		firstPrevWitness = &rootAsset.PrevWitnesses[0]
	}
	firstPrevWitness.TxWitness = witness

	return sweepVpkt, nil
}

// genSuccessBtcControlBlock generates the control block for the timeout path of
// the swap.
func (s *SwapOut) genSuccessBtcControlBlock(taprootAssetRoot []byte) (
	*txscript.ControlBlock, error) {

	internalKey, err := s.getAggregateKey()
	if err != nil {
		return nil, err
	}

	timeOutLeaf, err := s.GetTimeOutLeaf()
	if err != nil {
		return nil, err
	}

	timeOutLeafHash := timeOutLeaf.TapHash()

	btcControlBlock := &txscript.ControlBlock{
		LeafVersion:    txscript.BaseLeafVersion,
		InternalKey:    internalKey,
		InclusionProof: append(timeOutLeafHash[:], taprootAssetRoot[:]...),
	}

	successPathScript, err := s.GetSuccesScript()
	if err != nil {
		return nil, err
	}

	rootHash := btcControlBlock.RootHash(successPathScript)
	tapKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash)
	if tapKey.SerializeCompressed()[0] ==
		secp256k1.PubKeyFormatCompressedOdd {

		btcControlBlock.OutputKeyYIsOdd = true
	}

	return btcControlBlock, nil
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
