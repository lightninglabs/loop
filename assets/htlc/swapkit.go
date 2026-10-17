package htlc

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

// SwapKit holds information needed to facilitate an on-chain asset to offchain
// bitcoin atomic swap. The keys within the struct are the public keys of the
// sender and receiver that will be used to create the on-chain HTLC.
type SwapKit struct {
	// SenderPubKey is the public key of the sender for the joint key
	// that will be used to create the HTLC.
	SenderPubKey *btcec.PublicKey

	// ReceiverPubKey is the public key of the receiver that will be used to
	// create the HTLC.
	ReceiverPubKey *btcec.PublicKey

	// AssetID is the identifier of the asset that will be swapped.
	AssetID []byte

	// Amount is the amount of the asset that will be swapped.
	Amount uint64

	// SwapHash is the hash of the preimage in the  swap HTLC.
	SwapHash lntypes.Hash

	// CsvExpiry is the relative timelock in blocks for the swap.
	CsvExpiry uint32

	// AddressParams is the chain parameters of the chain the deposit is
	// being created on.
	AddressParams *address.ChainParams

	// CheckCSV indicates whether the success path script should include a
	// CHECKSEQUENCEVERIFY check. This is used to prevent potential pinning
	// attacks when the HTLC is not part of a package relay.
	CheckCSV bool
}

// GetSuccessScript returns the success path script of the swap HTLC.
func (s *SwapKit) GetSuccessScript() ([]byte, error) {
	return GenSuccessPathScript(s.ReceiverPubKey, s.SwapHash, s.CheckCSV)
}

// GetTimeoutScript returns the timeout path script of the swap HTLC.
func (s *SwapKit) GetTimeoutScript() ([]byte, error) {
	return GenTimeoutPathScript(s.SenderPubKey, int64(s.CsvExpiry))
}

// GetAggregateKey returns the aggregate MuSig2 key used in the swap HTLC.
func (s *SwapKit) GetAggregateKey() (*btcec.PublicKey, error) {
	sortKeys := true
	aggregateKey, err := input.MuSig2CombineKeys(
		input.MuSig2Version100RC2,
		[]*btcec.PublicKey{
			s.SenderPubKey, s.ReceiverPubKey,
		},
		sortKeys,
		&input.MuSig2Tweaks{},
	)
	if err != nil {
		return nil, err
	}

	return aggregateKey.PreTweakedKey, nil
}

// GetTimeOutLeaf returns the timeout leaf of the swap.
func (s *SwapKit) GetTimeOutLeaf() (txscript.TapLeaf, error) {
	timeoutScript, err := s.GetTimeoutScript()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	timeoutLeaf := txscript.NewBaseTapLeaf(timeoutScript)

	return timeoutLeaf, nil
}

// GetSuccessLeaf returns the success leaf of the swap.
func (s *SwapKit) GetSuccessLeaf() (txscript.TapLeaf, error) {
	successScript, err := s.GetSuccessScript()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	successLeaf := txscript.NewBaseTapLeaf(successScript)

	return successLeaf, nil
}

// GetSiblingPreimage returns the sibling preimage of the HTLC bitcoin top level
// output.
func (s *SwapKit) GetSiblingPreimage() (commitment.TapscriptPreimage, error) {
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

// CreateHtlcVpkt creates the vpacket for the HTLC.
func (s *SwapKit) CreateHtlcVpkt() (*tappsbt.VPacket, error) {
	assetId := asset.ID{}
	copy(assetId[:], s.AssetID)

	btcInternalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}

	siblingPreimage, err := s.GetSiblingPreimage()
	if err != nil {
		return nil, err
	}

	tapScriptKey, _, _, _, err := CreateOpTrueLeaf()
	if err != nil {
		return nil, err
	}

	pkt := &tappsbt.VPacket{
		Inputs: []*tappsbt.VInput{{
			PrevID: asset.PrevID{
				ID: assetId,
			},
		}},
		Outputs:     make([]*tappsbt.VOutput, 0, 2),
		ChainParams: s.AddressParams,
		Version:     tappsbt.V1,
	}
	pkt.Outputs = append(pkt.Outputs, &tappsbt.VOutput{
		Amount:            0,
		Type:              tappsbt.TypeSplitRoot,
		AnchorOutputIndex: 0,
		ScriptKey:         asset.NUMSScriptKey,
	})
	pkt.Outputs = append(pkt.Outputs, &tappsbt.VOutput{
		AssetVersion:      asset.V1,
		Amount:            s.Amount,
		AnchorOutputIndex: 1,
		ScriptKey: asset.NewScriptKey(
			tapScriptKey.PubKey,
		),
		AnchorOutputInternalKey:      btcInternalKey,
		AnchorOutputTapscriptSibling: &siblingPreimage,
	})

	return pkt, nil
}

// GenTimeoutBtcControlBlock generates the control block for the timeout path of
// the swap.
func (s *SwapKit) GenTimeoutBtcControlBlock(taprootAssetRoot []byte) (
	*txscript.ControlBlock, error) {

	internalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}

	successLeaf, err := s.GetSuccessLeaf()
	if err != nil {
		return nil, err
	}

	successLeafHash := successLeaf.TapHash()

	btcControlBlock := &txscript.ControlBlock{
		InternalKey: internalKey,
		LeafVersion: txscript.BaseLeafVersion,
		InclusionProof: append(
			successLeafHash[:], taprootAssetRoot...,
		),
	}

	timeoutPathScript, err := s.GetTimeoutScript()
	if err != nil {
		return nil, err
	}

	rootHash := btcControlBlock.RootHash(timeoutPathScript)
	tapKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash)
	if tapKey.SerializeCompressed()[0] ==
		secp256k1.PubKeyFormatCompressedOdd {

		btcControlBlock.OutputKeyYIsOdd = true
	}

	return btcControlBlock, nil
}

// GenSuccessBtcControlBlock generates the control block for the timeout path of
// the swap.
func (s *SwapKit) GenSuccessBtcControlBlock(taprootAssetRoot []byte) (
	*txscript.ControlBlock, error) {

	internalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}

	timeOutLeaf, err := s.GetTimeOutLeaf()
	if err != nil {
		return nil, err
	}

	timeOutLeafHash := timeOutLeaf.TapHash()

	btcControlBlock := &txscript.ControlBlock{
		InternalKey: internalKey,
		LeafVersion: txscript.BaseLeafVersion,
		InclusionProof: append(
			timeOutLeafHash[:], taprootAssetRoot...,
		),
	}

	successPathScript, err := s.GetSuccessScript()
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

// GenTaprootAssetRootFromProof generates the taproot asset root from the proof
// of the swap.
func GenTaprootAssetRootFromProof(p *proof.Proof) ([]byte, error) {
	tapCommitment, err := p.VerifyProofs()
	if err != nil {
		return nil, err
	}

	taprootAssetRoot := tapCommitment.TapscriptRoot(nil)

	return taprootAssetRoot[:], nil
}

// GetPkScriptFromAsset returns the toplevel bitcoin script with the given
// asset.
func (s *SwapKit) GetPkScriptFromAsset(asset *asset.Asset) ([]byte, error) {
	assetCopy := asset.CopySpendTemplate()

	version := commitment.TapCommitmentV2
	assetCommitment, err := commitment.FromAssets(
		&version, assetCopy,
	)
	if err != nil {
		return nil, err
	}

	assetCommitment, err = commitment.TrimSplitWitnesses(
		&version, assetCommitment,
	)
	if err != nil {
		return nil, err
	}

	siblingPreimage, err := s.GetSiblingPreimage()
	if err != nil {
		return nil, err
	}

	siblingHash, err := siblingPreimage.TapHash()
	if err != nil {
		return nil, err
	}

	btcInternalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}

	return tapscript.PayToAddrScript(
		*btcInternalKey, siblingHash, *assetCommitment,
	)
}

// CreatePreimageWitness creates a preimage witness for the swap.
func (s *SwapKit) CreatePreimageWitness(ctx context.Context,
	signer lndclient.SignerClient, htlcProof *proof.Proof,
	sweepBtcPacket *psbt.Packet, keyLocator keychain.KeyLocator,
	preimage lntypes.Preimage) (wire.TxWitness, error) {

	assetTxOut := &wire.TxOut{
		PkScript: sweepBtcPacket.Inputs[0].WitnessUtxo.PkScript,
		Value:    sweepBtcPacket.Inputs[0].WitnessUtxo.Value,
	}
	feeTxOut := &wire.TxOut{
		PkScript: sweepBtcPacket.Inputs[1].WitnessUtxo.PkScript,
		Value:    sweepBtcPacket.Inputs[1].WitnessUtxo.Value,
	}

	if s.CheckCSV {
		sweepBtcPacket.UnsignedTx.TxIn[0].Sequence = 1
	}

	successScript, err := s.GetSuccessScript()
	if err != nil {
		return nil, err
	}

	signDesc := &lndclient.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			KeyLocator: keyLocator,
		},
		SignMethod:    input.TaprootScriptSpendSignMethod,
		WitnessScript: successScript,
		Output:        assetTxOut,
		InputIndex:    0,
	}
	sig, err := signer.SignOutputRaw(
		ctx, sweepBtcPacket.UnsignedTx,
		[]*lndclient.SignDescriptor{
			signDesc,
		},
		[]*wire.TxOut{
			assetTxOut, feeTxOut,
		},
	)
	if err != nil {
		return nil, err
	}

	taprootAssetRoot, err := GenTaprootAssetRootFromProof(htlcProof)
	if err != nil {
		return nil, err
	}

	successControlBlock, err := s.GenSuccessBtcControlBlock(
		taprootAssetRoot,
	)
	if err != nil {
		return nil, err
	}

	controlBlockBytes, err := successControlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return wire.TxWitness{
		preimage[:],
		sig[0],
		successScript,
		controlBlockBytes,
	}, nil
}

// CreateTimeoutWitness creates a timeout witness for the swap.
func (s *SwapKit) CreateTimeoutWitness(ctx context.Context,
	signer lndclient.SignerClient, htlcProof *proof.Proof,
	sweepBtcPacket *psbt.Packet, keyLocator keychain.KeyLocator) (
	wire.TxWitness, error) {

	assetTxOut := &wire.TxOut{
		PkScript: sweepBtcPacket.Inputs[0].WitnessUtxo.PkScript,
		Value:    sweepBtcPacket.Inputs[0].WitnessUtxo.Value,
	}
	feeTxOut := &wire.TxOut{
		PkScript: sweepBtcPacket.Inputs[1].WitnessUtxo.PkScript,
		Value:    sweepBtcPacket.Inputs[1].WitnessUtxo.Value,
	}

	sweepBtcPacket.UnsignedTx.TxIn[0].Sequence = s.CsvExpiry

	timeoutScript, err := s.GetTimeoutScript()
	if err != nil {
		return nil, err
	}

	signDesc := &lndclient.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			KeyLocator: keyLocator,
		},
		SignMethod:    input.TaprootScriptSpendSignMethod,
		WitnessScript: timeoutScript,
		Output:        assetTxOut,
		InputIndex:    0,
	}
	sig, err := signer.SignOutputRaw(
		ctx, sweepBtcPacket.UnsignedTx,
		[]*lndclient.SignDescriptor{
			signDesc,
		},
		[]*wire.TxOut{
			assetTxOut, feeTxOut,
		},
	)
	if err != nil {
		return nil, err
	}

	taprootAssetRoot, err := GenTaprootAssetRootFromProof(htlcProof)
	if err != nil {
		return nil, err
	}

	timeoutControlBlock, err := s.GenTimeoutBtcControlBlock(
		taprootAssetRoot,
	)
	if err != nil {
		return nil, err
	}

	controlBlockBytes, err := timeoutControlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return wire.TxWitness{
		sig[0],
		timeoutScript,
		controlBlockBytes,
	}, nil
}
