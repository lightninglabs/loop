package deposit

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
)

// AddressProofClient is the narrow tapd boundary needed to create deposit
// addresses and export their proofs.
type AddressProofClient interface {
	NewAddr(context.Context, *taprpc.NewAddrRequest,
		...grpc.CallOption) (*taprpc.Addr, error)
	ExportProof(context.Context, *taprpc.ExportProofRequest,
		...grpc.CallOption) (*taprpc.ProofFile, error)
}

// Kit contains the immutable information needed to create and operate a
// two-party MuSig2 asset deposit.
type Kit struct {
	funderKey   *btcec.PublicKey
	coSignerKey *btcec.PublicKey
	keyLocator  keychain.KeyLocator
	assetID     asset.ID
	csvExpiry   uint32
	muSig2Key   *musig2.AggregateKey
	chainParams *address.ChainParams
}

// NewKit creates a validated legacy asset deposit kit.
func NewKit(funderKey, coSignerKey *btcec.PublicKey,
	keyLocator keychain.KeyLocator, assetID asset.ID, csvExpiry uint32,
	chainParams *address.ChainParams) (*Kit, error) {

	if funderKey == nil {
		return nil, fmt.Errorf("funder public key is required")
	}
	if coSignerKey == nil {
		return nil, fmt.Errorf("co-signer public key is required")
	}
	if funderKey.IsEqual(coSignerKey) {
		return nil, fmt.Errorf("funder and co-signer keys must differ")
	}
	if assetID == (asset.ID{}) {
		return nil, fmt.Errorf("asset ID is required")
	}
	if csvExpiry == 0 {
		return nil, fmt.Errorf("CSV expiry must be positive")
	}
	if csvExpiry > wire.SequenceLockTimeMask {
		return nil, fmt.Errorf("CSV expiry exceeds block-based BIP68 range")
	}
	if chainParams == nil || chainParams.Params == nil ||
		chainParams.TapHRP == "" {

		return nil, fmt.Errorf("address parameters are incomplete")
	}

	funderKeyCopy, err := btcec.ParsePubKey(
		funderKey.SerializeCompressed(),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid funder public key: %w", err)
	}
	coSignerKeyCopy, err := btcec.ParsePubKey(
		coSignerKey.SerializeCompressed(),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid co-signer public key: %w", err)
	}

	sortKeys := true
	muSig2Key, err := input.MuSig2CombineKeys(
		input.MuSig2Version100RC2,
		[]*btcec.PublicKey{funderKeyCopy, coSignerKeyCopy}, sortKeys,
		&input.MuSig2Tweaks{TaprootBIP0086Tweak: true},
	)
	if err != nil {
		return nil, err
	}

	return &Kit{
		funderKey:   funderKeyCopy,
		coSignerKey: coSignerKeyCopy,
		keyLocator:  keyLocator,
		assetID:     assetID,
		csvExpiry:   csvExpiry,
		muSig2Key:   muSig2Key,
		chainParams: cloneAddressParams(*chainParams),
	}, nil
}

func cloneAddressParams(params address.ChainParams) *address.ChainParams {
	paramsCopy := params
	bitcoinParams := *params.Params
	paramsCopy.Params = &bitcoinParams

	return &paramsCopy
}

// GenTimeoutPathScript constructs the deposit funder's CSV timeout script.
//
//	<funderKey> OP_CHECKSIGVERIFY <csvExpiry> OP_CHECKSEQUENCEVERIFY
func (d *Kit) GenTimeoutPathScript() ([]byte, error) {
	if d == nil || d.funderKey == nil {
		return nil, fmt.Errorf("deposit kit is incomplete")
	}

	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(d.funderKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddInt64(int64(d.csvExpiry))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	return builder.Script()
}

func (d *Kit) timeoutPathSibling() (*commitment.TapscriptPreimage, error) {
	timeoutScript, err := d.GenTimeoutPathScript()
	if err != nil {
		return nil, err
	}

	btcTapLeaf := txscript.NewBaseTapLeaf(timeoutScript)
	sibling, err := commitment.NewPreimageFromLeaf(btcTapLeaf)
	if err != nil {
		return nil, err
	}

	return sibling, nil
}

func (d *Kit) encodedTimeoutPathSibling() ([]byte, error) {
	sibling, err := d.timeoutPathSibling()
	if err != nil {
		return nil, err
	}

	siblingBytes, _, err := commitment.MaybeEncodeTapscriptPreimage(sibling)
	if err != nil {
		return nil, err
	}

	return siblingBytes, nil
}

// NewAddr creates a two-party MuSig2 deposit address with a unilateral funder
// timeout path.
func (d *Kit) NewAddr(ctx context.Context, client AddressProofClient,
	amount uint64) (*taprpc.Addr, error) {

	if d == nil {
		return nil, fmt.Errorf("deposit kit is required")
	}
	if client == nil {
		return nil, fmt.Errorf("tapd client is required")
	}
	if amount == 0 {
		return nil, fmt.Errorf("deposit amount must be positive")
	}

	siblingBytes, err := d.encodedTimeoutPathSibling()
	if err != nil {
		return nil, err
	}
	tapScriptKey, _, _, _, err := htlc.CreateOpTrueLeaf()
	if err != nil {
		return nil, err
	}

	return client.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId:   d.assetID[:],
		Amt:       amount,
		ScriptKey: rpcutils.MarshalScriptKey(tapScriptKey),
		InternalKey: &taprpc.KeyDescriptor{
			RawKeyBytes: d.muSig2Key.PreTweakedKey.SerializeCompressed(),
		},
		TapscriptSibling: siblingBytes,
	})
}

// NewHtlcAddr creates an HTLC address using the deposit parties as sender and
// receiver under the immutable legacy deposit policy.
func (d *Kit) NewHtlcAddr(ctx context.Context, client AddressProofClient,
	amount uint64, swapHash lntypes.Hash, csvExpiry uint32) (
	*taprpc.Addr, *htlc.SwapKit, error) {

	if d == nil {
		return nil, nil, fmt.Errorf("deposit kit is required")
	}
	if client == nil {
		return nil, nil, fmt.Errorf("tapd client is required")
	}

	swapKit, err := d.newHtlcSwapKit(amount, swapHash, csvExpiry)
	if err != nil {
		return nil, nil, err
	}
	btcInternalKey, err := swapKit.GetAggregateKey()
	if err != nil {
		return nil, nil, err
	}
	sibling, err := swapKit.GetSiblingPreimage()
	if err != nil {
		return nil, nil, err
	}
	siblingBytes, _, err := commitment.MaybeEncodeTapscriptPreimage(&sibling)
	if err != nil {
		return nil, nil, err
	}
	tapScriptKey, _, _, _, err := htlc.CreateOpTrueLeaf()
	if err != nil {
		return nil, nil, err
	}

	htlcAddr, err := client.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId:   d.assetID[:],
		Amt:       amount,
		ScriptKey: rpcutils.MarshalScriptKey(tapScriptKey),
		InternalKey: &taprpc.KeyDescriptor{
			RawKeyBytes: btcInternalKey.SerializeCompressed(),
		},
		TapscriptSibling: siblingBytes,
	})
	if err != nil {
		return nil, nil, err
	}

	return htlcAddr, swapKit, nil
}

func (d *Kit) newHtlcSwapKit(amount uint64, swapHash lntypes.Hash,
	csvExpiry uint32) (*htlc.SwapKit, error) {

	return htlc.NewSwapKit(htlc.LegacyDepositV0, htlc.Params{
		SenderPubKey:   d.funderKey,
		ReceiverPubKey: d.coSignerKey,
		AssetID:        d.assetID,
		Amount:         amount,
		SwapHash:       swapHash,
		CsvExpiry:      csvExpiry,
		AddressParams:  d.chainParams,
	})
}

// TapScriptKey returns the OP_TRUE asset script key used by deposits.
func (d *Kit) TapScriptKey() (asset.ScriptKey, error) {
	tapScriptKey, _, _, _, err := htlc.CreateOpTrueLeaf()
	if err != nil {
		return asset.ScriptKey{}, err
	}

	return asset.NewScriptKey(tapScriptKey.PubKey), nil
}

// ExportProof exports the proof for an exact deposit outpoint.
func (d *Kit) ExportProof(ctx context.Context, client AddressProofClient,
	outpoint *wire.OutPoint) (*taprpc.ProofFile, error) {

	if d == nil {
		return nil, fmt.Errorf("deposit kit is required")
	}
	if client == nil {
		return nil, fmt.Errorf("tapd client is required")
	}
	if outpoint == nil {
		return nil, fmt.Errorf("deposit outpoint is required")
	}
	scriptKey, err := d.TapScriptKey()
	if err != nil {
		return nil, err
	}

	return client.ExportProof(ctx, &taprpc.ExportProofRequest{
		AssetId:   d.assetID[:],
		ScriptKey: scriptKey.PubKey.SerializeCompressed(),
		Outpoint: &taprpc.OutPoint{
			Txid:        outpoint.Hash[:],
			OutputIndex: outpoint.Index,
		},
	})
}

func (d *Kit) validateAsset(depositAsset *asset.Asset) error {
	if depositAsset == nil {
		return fmt.Errorf("deposit asset is required")
	}
	proofID := depositAsset.Genesis.ID()
	if !bytes.Equal(d.assetID[:], proofID[:]) {
		return fmt.Errorf("asset proof ID does not match deposit")
	}
	if depositAsset.Amount == 0 {
		return fmt.Errorf("deposit asset amount must be positive")
	}
	if depositAsset.Version != asset.V0 && depositAsset.Version != asset.V1 {
		return fmt.Errorf("unsupported legacy deposit asset version")
	}
	if depositAsset.ScriptVersion != asset.ScriptV0 {
		return fmt.Errorf("asset proof script version does not match deposit")
	}
	if depositAsset.LockTime != 0 || depositAsset.RelativeLockTime != 0 {
		return fmt.Errorf("asset proof contains an unexpected locktime")
	}

	expectedScriptKey, err := d.TapScriptKey()
	if err != nil {
		return err
	}
	if depositAsset.ScriptKey.PubKey == nil ||
		!depositAsset.ScriptKey.PubKey.IsEqual(expectedScriptKey.PubKey) {

		return fmt.Errorf("asset proof script key does not match deposit")
	}

	return nil
}

func (d *Kit) validateProof(depositProof *proof.Proof) (
	*commitment.TapCommitment, error) {

	if d == nil {
		return nil, fmt.Errorf("deposit kit is required")
	}
	if depositProof == nil {
		return nil, fmt.Errorf("deposit proof is required")
	}
	if err := d.validateAsset(&depositProof.Asset); err != nil {
		return nil, err
	}

	tapCommitment, err := depositProof.VerifyProofs()
	if err != nil {
		return nil, fmt.Errorf("invalid deposit proof: %w", err)
	}
	internalKey := depositProof.InclusionProof.InternalKey
	if internalKey == nil ||
		!internalKey.IsEqual(d.muSig2Key.PreTweakedKey) {

		return nil, fmt.Errorf("deposit internal key mismatch")
	}
	commitmentProof := depositProof.InclusionProof.CommitmentProof
	if commitmentProof == nil ||
		commitmentProof.TapSiblingPreimage == nil {

		return nil, fmt.Errorf("deposit sibling preimage is required")
	}
	expectedSibling, err := d.encodedTimeoutPathSibling()
	if err != nil {
		return nil, err
	}
	actualSibling, _, err := commitment.MaybeEncodeTapscriptPreimage(
		commitmentProof.TapSiblingPreimage,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid deposit sibling preimage: %w", err)
	}
	if !bytes.Equal(expectedSibling, actualSibling) {
		return nil, fmt.Errorf("deposit sibling preimage mismatch")
	}

	return tapCommitment, nil
}

// VerifyProof verifies the proof commitment and binds it to this deposit's
// asset, internal key, and timeout sibling. It returns the complete anchor
// Taproot Merkle root used to tweak the MuSig2 key for a key-path spend.
func (d *Kit) VerifyProof(depositProof *proof.Proof) ([]byte, error) {
	tapCommitment, err := d.validateProof(depositProof)
	if err != nil {
		return nil, err
	}
	sibling, err := d.timeoutPathSibling()
	if err != nil {
		return nil, err
	}
	siblingHash, err := sibling.TapHash()
	if err != nil {
		return nil, err
	}
	root := tapCommitment.TapscriptRoot(siblingHash)

	return append([]byte(nil), root[:]...), nil
}

// GenTimeoutBtcControlBlock creates the deposit timeout-path control block.
func (d *Kit) GenTimeoutBtcControlBlock(taprootAssetRoot []byte) (
	*txscript.ControlBlock, error) {

	if d == nil || d.muSig2Key == nil {
		return nil, fmt.Errorf("deposit kit is incomplete")
	}
	if len(taprootAssetRoot) != chainhash.HashSize {
		return nil, fmt.Errorf("asset root must be %d bytes",
			chainhash.HashSize)
	}
	controlBlock := &txscript.ControlBlock{
		InternalKey:    d.muSig2Key.PreTweakedKey,
		LeafVersion:    txscript.BaseLeafVersion,
		InclusionProof: append([]byte(nil), taprootAssetRoot...),
	}
	timeoutScript, err := d.GenTimeoutPathScript()
	if err != nil {
		return nil, err
	}
	rootHash := controlBlock.RootHash(timeoutScript)
	tapKey := txscript.ComputeTaprootOutputKey(
		d.muSig2Key.PreTweakedKey, rootHash,
	)
	controlBlock.OutputKeyYIsOdd = tapKey.SerializeCompressed()[0] ==
		secp256k1.PubKeyFormatCompressedOdd

	return controlBlock, nil
}

type validatedSweep struct {
	assetInputIndex int
	prevOutputs     []*wire.TxOut
	assetRoot       []byte
}

func findUniqueInput(tx *wire.MsgTx, outpoint wire.OutPoint) (int, error) {
	inputIndex := -1
	for idx, txIn := range tx.TxIn {
		if txIn == nil {
			return 0, fmt.Errorf("sweep input %d is nil", idx)
		}
		if txIn.PreviousOutPoint != outpoint {
			continue
		}
		if inputIndex >= 0 {
			return 0, fmt.Errorf("proof outpoint appears more than once")
		}
		inputIndex = idx
	}
	if inputIndex < 0 {
		return 0, fmt.Errorf("asset input does not spend proof outpoint")
	}

	return inputIndex, nil
}

func (d *Kit) validateSweep(depositProof *proof.Proof,
	sweepPacket *psbt.Packet) (*validatedSweep, error) {

	tapCommitment, err := d.validateProof(depositProof)
	if err != nil {
		return nil, err
	}
	if sweepPacket == nil || sweepPacket.UnsignedTx == nil {
		return nil, fmt.Errorf("sweep PSBT is required")
	}
	if len(sweepPacket.UnsignedTx.TxIn) == 0 ||
		len(sweepPacket.UnsignedTx.TxIn) != len(sweepPacket.Inputs) {

		return nil, fmt.Errorf("sweep PSBT input metadata is incomplete")
	}
	if sweepPacket.UnsignedTx.Version < 2 {
		return nil, fmt.Errorf("sweep transaction must use version 2")
	}

	assetInputIndex, err := findUniqueInput(
		sweepPacket.UnsignedTx, depositProof.OutPoint(),
	)
	if err != nil {
		return nil, err
	}
	prevOutputs := make([]*wire.TxOut, len(sweepPacket.Inputs))
	for idx := range sweepPacket.Inputs {
		witnessUtxo := sweepPacket.Inputs[idx].WitnessUtxo
		if witnessUtxo == nil {
			return nil, fmt.Errorf(
				"sweep input %d requires a witness UTXO", idx,
			)
		}
		prevOutputs[idx] = &wire.TxOut{
			Value: witnessUtxo.Value,
			PkScript: append(
				[]byte(nil), witnessUtxo.PkScript...,
			),
		}
	}

	proofOutputIndex := int(depositProof.InclusionProof.OutputIndex)
	if proofOutputIndex >= len(depositProof.AnchorTx.TxOut) {
		return nil, fmt.Errorf("proof output index is out of bounds")
	}
	proofTxOut := depositProof.AnchorTx.TxOut[proofOutputIndex]
	if proofTxOut == nil {
		return nil, fmt.Errorf("proof anchor output is nil")
	}
	assetPrevOut := prevOutputs[assetInputIndex]
	if proofTxOut.Value != assetPrevOut.Value ||
		!bytes.Equal(proofTxOut.PkScript, assetPrevOut.PkScript) {

		return nil, fmt.Errorf("asset prevout does not match proof anchor")
	}

	sibling, err := d.timeoutPathSibling()
	if err != nil {
		return nil, err
	}
	siblingHash, err := sibling.TapHash()
	if err != nil {
		return nil, err
	}
	expectedPkScript, err := tapscript.PayToAddrScript(
		*d.muSig2Key.PreTweakedKey, siblingHash, *tapCommitment,
	)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(expectedPkScript, assetPrevOut.PkScript) {
		return nil, fmt.Errorf("asset input script does not match proof")
	}

	assetRootHash := tapCommitment.TapscriptRoot(nil)
	return &validatedSweep{
		assetInputIndex: assetInputIndex,
		prevOutputs:     prevOutputs,
		assetRoot:       append([]byte(nil), assetRootHash[:]...),
	}, nil
}

func verifyTapscriptSignature(tx *wire.MsgTx, sweep *validatedSweep,
	witnessScript []byte, pubKey *btcec.PublicKey, signature []byte) error {

	parsedSignature, err := schnorr.ParseSignature(signature)
	if err != nil {
		return fmt.Errorf("signer returned an invalid Schnorr signature: %w",
			err)
	}
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for idx, txIn := range tx.TxIn {
		prevOutFetcher.AddPrevOut(
			txIn.PreviousOutPoint, sweep.prevOutputs[idx],
		)
	}
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	tapLeaf := txscript.NewBaseTapLeaf(witnessScript)
	sigHash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, tx,
		sweep.assetInputIndex, prevOutFetcher, tapLeaf,
	)
	if err != nil {
		return fmt.Errorf("unable to calculate asset input sighash: %w", err)
	}
	if !parsedSignature.Verify(sigHash, pubKey) {
		return fmt.Errorf("signer returned a signature for the wrong key")
	}

	return nil
}

// CreateTimeoutWitness signs the exact proof-bound deposit input and returns
// its input index and timeout-path witness. The method sets the required CSV
// sequence on that input before signing.
func (d *Kit) CreateTimeoutWitness(ctx context.Context,
	signer lndclient.SignerClient, depositProof *proof.Proof,
	sweepPacket *psbt.Packet) (*htlc.SpendWitness, error) {

	if signer == nil {
		return nil, fmt.Errorf("signer is required")
	}
	sweep, err := d.validateSweep(depositProof, sweepPacket)
	if err != nil {
		return nil, err
	}
	sweepTx := sweepPacket.UnsignedTx.Copy()
	sweepTx.TxIn[sweep.assetInputIndex].Sequence = d.csvExpiry
	timeoutScript, err := d.GenTimeoutPathScript()
	if err != nil {
		return nil, err
	}

	signDesc := &lndclient.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			KeyLocator: d.keyLocator,
		},
		SignMethod:    input.TaprootScriptSpendSignMethod,
		WitnessScript: timeoutScript,
		Output:        sweep.prevOutputs[sweep.assetInputIndex],
		InputIndex:    sweep.assetInputIndex,
	}
	sigs, err := signer.SignOutputRaw(
		ctx, sweepTx,
		[]*lndclient.SignDescriptor{signDesc}, sweep.prevOutputs,
	)
	if err != nil {
		return nil, err
	}
	if len(sigs) != 1 || len(sigs[0]) == 0 {
		return nil, fmt.Errorf("signer returned an invalid signature set")
	}
	if err := verifyTapscriptSignature(
		sweepTx, sweep, timeoutScript, d.funderKey,
		sigs[0],
	); err != nil {
		return nil, err
	}

	controlBlock, err := d.GenTimeoutBtcControlBlock(sweep.assetRoot)
	if err != nil {
		return nil, err
	}
	controlBlockBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, err
	}
	sweepPacket.UnsignedTx.TxIn[sweep.assetInputIndex].Sequence = d.csvExpiry

	return &htlc.SpendWitness{
		InputIndex: uint32(sweep.assetInputIndex),
		Witness: wire.TxWitness{
			sigs[0], timeoutScript, controlBlockBytes,
		},
	}, nil
}
