package htlc

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
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

const (
	// SuccessSequence is the relative lock used by the success path. It is
	// part of the legacy version-zero policy.
	SuccessSequence uint32 = 1

	// rawSimNetHDCoinType is the BIP-0044 coin type used by btcd's native
	// simnet parameters. LND deliberately uses the testnet coin type for
	// simnet instead, so both forms need explicit, stable definitions.
	rawSimNetHDCoinType uint32 = 115
)

// Policy identifies an immutable version of an asset HTLC contract. New
// protocols must use distinct policies so they cannot silently change an
// existing on-chain contract.
type Policy uint8

const (
	// PolicyUnknown reserves the zero value so every caller must choose the
	// protocol whose compatibility rules it expects.
	PolicyUnknown Policy = iota

	// LegacyDepositV0 preserves the asset deposit HTLC contract that predates
	// this shared package.
	LegacyDepositV0
)

// Params contains the validated inputs used to construct a SwapKit.
type Params struct {
	SenderPubKey   *btcec.PublicKey
	ReceiverPubKey *btcec.PublicKey
	AssetID        asset.ID
	Amount         uint64
	SwapHash       lntypes.Hash
	CsvExpiry      uint32
	AddressParams  *address.ChainParams
}

// SwapKit holds the immutable information required to construct and spend an
// on-chain Taproot Asset HTLC. NewSwapKit is the only constructor so a funded
// contract cannot later be changed by mutating a key, hash, amount, or expiry.
type SwapKit struct {
	policy         Policy
	senderPubKey   *btcec.PublicKey
	receiverPubKey *btcec.PublicKey
	assetID        asset.ID
	amount         uint64
	swapHash       lntypes.Hash
	csvExpiry      uint32
	addressParams  address.ChainParams
}

// NewSwapKit returns a defensively validated asset HTLC kit.
func NewSwapKit(policy Policy, params Params) (*SwapKit, error) {
	if params.SenderPubKey == nil {
		return nil, fmt.Errorf("sender public key is required")
	}
	if params.ReceiverPubKey == nil {
		return nil, fmt.Errorf("receiver public key is required")
	}
	if params.AddressParams == nil {
		return nil, fmt.Errorf("address parameters are required")
	}
	if params.AddressParams.Params == nil || params.AddressParams.TapHRP == "" {
		return nil, fmt.Errorf("address parameters are incomplete")
	}
	canonicalParams, err := canonicalAddressParams(params.AddressParams)
	if err != nil {
		return nil, err
	}

	senderKey, err := btcec.ParsePubKey(
		params.SenderPubKey.SerializeCompressed(),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid sender public key: %w", err)
	}
	receiverKey, err := btcec.ParsePubKey(
		params.ReceiverPubKey.SerializeCompressed(),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid receiver public key: %w", err)
	}
	if senderKey.IsEqual(receiverKey) {
		return nil, fmt.Errorf("sender and receiver keys must differ")
	}

	addressParams := cloneAddressParams(*canonicalParams)
	kit := &SwapKit{
		policy:         policy,
		senderPubKey:   senderKey,
		receiverPubKey: receiverKey,
		assetID:        params.AssetID,
		amount:         params.Amount,
		swapHash:       params.SwapHash,
		csvExpiry:      params.CsvExpiry,
		addressParams:  *addressParams,
	}
	if err := kit.Validate(); err != nil {
		return nil, err
	}

	return kit, nil
}

func canonicalAddressParams(params *address.ChainParams) (
	*address.ChainParams, error) {

	rawSimNet := cloneAddressParams(address.SimNetTap)
	rawSimNet.HDCoinType = rawSimNetHDCoinType
	lndSimNet := cloneAddressParams(address.SimNetTap)
	lndSimNet.HDCoinType = address.TestNet3Tap.HDCoinType

	knownNetworks := []*address.ChainParams{
		&address.MainNetTap,
		&address.TestNet3Tap,
		&address.TestNet4Tap,
		&address.RegressionNetTap,
		&address.SigNetTap,
		rawSimNet,
		lndSimNet,
	}
	for _, known := range knownNetworks {
		if params.TapHRP != known.TapHRP || params.Net != known.Net ||
			params.HDCoinType != known.HDCoinType {

			continue
		}

		return cloneAddressParams(*known), nil
	}

	return nil, fmt.Errorf("unsupported or mismatched address parameters")
}

// Policy returns the immutable contract policy.
func (s *SwapKit) Policy() Policy {
	return s.policy
}

// SenderPubKey returns the sender's refund public key.
func (s *SwapKit) SenderPubKey() *btcec.PublicKey {
	key, _ := btcec.ParsePubKey(s.senderPubKey.SerializeCompressed())

	return key
}

// ReceiverPubKey returns the receiver's claim public key.
func (s *SwapKit) ReceiverPubKey() *btcec.PublicKey {
	key, _ := btcec.ParsePubKey(s.receiverPubKey.SerializeCompressed())

	return key
}

// AssetID returns the exact asset identifier.
func (s *SwapKit) AssetID() asset.ID {
	return s.assetID
}

// Amount returns the amount in the asset's native unit.
func (s *SwapKit) Amount() uint64 {
	return s.amount
}

// SwapHash returns the payment hash committed to by the success path.
func (s *SwapKit) SwapHash() lntypes.Hash {
	return s.swapHash
}

// CsvExpiry returns the sender's block-based relative refund delay.
func (s *SwapKit) CsvExpiry() uint32 {
	return s.csvExpiry
}

// AddressParams returns a copy of the contract's address parameters.
func (s *SwapKit) AddressParams() address.ChainParams {
	return *cloneAddressParams(s.addressParams)
}

func cloneAddressParams(params address.ChainParams) *address.ChainParams {
	paramsCopy := params
	if params.Params != nil {
		bitcoinParams := *params.Params
		paramsCopy.Params = &bitcoinParams
	}

	return &paramsCopy
}

// Validate verifies every field required to create an HTLC virtual packet.
func (s *SwapKit) Validate() error {
	if err := s.validateScripts(); err != nil {
		return err
	}
	if s.amount == 0 {
		return fmt.Errorf("asset amount must be positive")
	}
	if s.assetID == (asset.ID{}) {
		return fmt.Errorf("asset ID is required")
	}
	if s.swapHash == (lntypes.Hash{}) {
		return fmt.Errorf("swap hash is required")
	}

	return nil
}

func (s *SwapKit) validateScripts() error {
	if s == nil {
		return fmt.Errorf("swap kit is required")
	}
	if s.policy != LegacyDepositV0 {
		return fmt.Errorf("unknown asset HTLC policy: %d", s.policy)
	}
	if s.senderPubKey == nil {
		return fmt.Errorf("sender public key is required")
	}
	if s.receiverPubKey == nil {
		return fmt.Errorf("receiver public key is required")
	}
	if s.csvExpiry <= SuccessSequence {
		return fmt.Errorf("CSV expiry must exceed success sequence")
	}
	if s.csvExpiry > wire.SequenceLockTimeMask {
		return fmt.Errorf("CSV expiry exceeds block-based BIP68 range")
	}

	return nil
}

// GetSuccessScript returns the success path script of the swap HTLC.
func (s *SwapKit) GetSuccessScript() ([]byte, error) {
	if s == nil || s.receiverPubKey == nil {
		return nil, fmt.Errorf("receiver public key is required")
	}
	if s.policy != LegacyDepositV0 {
		return nil, fmt.Errorf("unknown asset HTLC policy: %d", s.policy)
	}

	return GenSuccessPathScript(s.receiverPubKey, s.swapHash)
}

// GetTimeoutScript returns the timeout path script of the swap HTLC.
func (s *SwapKit) GetTimeoutScript() ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("swap kit is required")
	}
	if s.policy != LegacyDepositV0 {
		return nil, fmt.Errorf("unknown asset HTLC policy: %d", s.policy)
	}

	return GenTimeoutPathScript(s.senderPubKey, int64(s.csvExpiry))
}

// GetAggregateKey returns the sorted MuSig2 aggregate key used in the swap
// HTLC. The key is deliberately returned before any Taproot tweak is applied.
func (s *SwapKit) GetAggregateKey() (*btcec.PublicKey, error) {
	if err := s.validateScripts(); err != nil {
		return nil, err
	}

	aggregateKey, err := input.MuSig2CombineKeys(
		input.MuSig2Version100RC2,
		[]*btcec.PublicKey{s.senderPubKey, s.receiverPubKey}, true,
		&input.MuSig2Tweaks{},
	)
	if err != nil {
		return nil, err
	}

	return aggregateKey.PreTweakedKey, nil
}

// GetTimeoutLeaf returns the timeout leaf of the swap.
func (s *SwapKit) GetTimeoutLeaf() (txscript.TapLeaf, error) {
	timeoutScript, err := s.GetTimeoutScript()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(timeoutScript), nil
}

// GetTimeOutLeaf returns the timeout leaf of the swap. The historical method
// capitalization is retained while callers migrate to GetTimeoutLeaf.
func (s *SwapKit) GetTimeOutLeaf() (txscript.TapLeaf, error) {
	return s.GetTimeoutLeaf()
}

// GetSuccessLeaf returns the success leaf of the swap.
func (s *SwapKit) GetSuccessLeaf() (txscript.TapLeaf, error) {
	successScript, err := s.GetSuccessScript()
	if err != nil {
		return txscript.TapLeaf{}, err
	}

	return txscript.NewBaseTapLeaf(successScript), nil
}

// GetSiblingPreimage returns the branch preimage placed beside the Taproot
// Asset commitment in the Bitcoin output tree.
func (s *SwapKit) GetSiblingPreimage() (
	commitment.TapscriptPreimage, error) {

	timeoutLeaf, err := s.GetTimeoutLeaf()
	if err != nil {
		return commitment.TapscriptPreimage{}, err
	}

	successLeaf, err := s.GetSuccessLeaf()
	if err != nil {
		return commitment.TapscriptPreimage{}, err
	}

	branch := txscript.NewTapBranch(timeoutLeaf, successLeaf)

	return commitment.NewPreimageFromBranch(branch), nil
}

// CreateHtlcVpkt creates the version-one virtual packet for the HTLC. The
// split-root and HTLC output indices and their interactive flags are consensus
// with the existing server implementation.
func (s *SwapKit) CreateHtlcVpkt() (*tappsbt.VPacket, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

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
			PrevID: asset.PrevID{ID: s.assetID},
		}},
		Outputs:     make([]*tappsbt.VOutput, 0, 2),
		ChainParams: cloneAddressParams(s.addressParams),
		Version:     tappsbt.V1,
	}
	pkt.Outputs = append(pkt.Outputs, &tappsbt.VOutput{
		AssetVersion:      asset.V1,
		Amount:            0,
		Type:              tappsbt.TypeSplitRoot,
		AnchorOutputIndex: 0,
		ScriptKey:         asset.NUMSScriptKey,
		Interactive:       true,
	})
	pkt.Outputs = append(pkt.Outputs, &tappsbt.VOutput{
		AssetVersion:                 asset.V1,
		Amount:                       s.amount,
		Interactive:                  true,
		AnchorOutputIndex:            1,
		ScriptKey:                    asset.NewScriptKey(tapScriptKey.PubKey),
		AnchorOutputInternalKey:      btcInternalKey,
		AnchorOutputTapscriptSibling: &siblingPreimage,
	})

	return pkt, nil
}

// GenTimeoutBtcControlBlock generates the Bitcoin control block for the
// timeout path. The supplied root must be the exact Taproot Asset commitment
// root committed to by the anchor output.
func (s *SwapKit) GenTimeoutBtcControlBlock(taprootAssetRoot []byte) (
	*txscript.ControlBlock, error) {

	return s.genBtcControlBlock(false, taprootAssetRoot)
}

// GenSuccessBtcControlBlock generates the Bitcoin control block for the
// success path.
func (s *SwapKit) GenSuccessBtcControlBlock(taprootAssetRoot []byte) (
	*txscript.ControlBlock, error) {

	return s.genBtcControlBlock(true, taprootAssetRoot)
}

// GetPkScriptFromRoot returns the Bitcoin anchor script for an exact Taproot
// Asset commitment root.
func (s *SwapKit) GetPkScriptFromRoot(taprootAssetRoot []byte) (
	[]byte, error) {

	controlBlock, err := s.GenSuccessBtcControlBlock(taprootAssetRoot)
	if err != nil {
		return nil, err
	}
	successScript, err := s.GetSuccessScript()
	if err != nil {
		return nil, err
	}

	rootHash := controlBlock.RootHash(successScript)
	internalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}
	outputKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash)

	return txscript.PayToTaprootScript(outputKey)
}

func (s *SwapKit) genBtcControlBlock(success bool,
	taprootAssetRoot []byte) (*txscript.ControlBlock, error) {

	if len(taprootAssetRoot) != 32 {
		return nil, fmt.Errorf("Taproot Asset root must be 32 bytes")
	}

	internalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}

	timeoutLeaf, err := s.GetTimeoutLeaf()
	if err != nil {
		return nil, err
	}
	successLeaf, err := s.GetSuccessLeaf()
	if err != nil {
		return nil, err
	}

	spendLeaf := timeoutLeaf
	siblingLeaf := successLeaf
	if success {
		spendLeaf = successLeaf
		siblingLeaf = timeoutLeaf
	}

	siblingHash := siblingLeaf.TapHash()
	inclusionProof := make([]byte, 0, 64)
	inclusionProof = append(inclusionProof, siblingHash[:]...)
	inclusionProof = append(inclusionProof, taprootAssetRoot...)

	controlBlock := &txscript.ControlBlock{
		InternalKey:    internalKey,
		LeafVersion:    txscript.BaseLeafVersion,
		InclusionProof: inclusionProof,
	}

	rootHash := controlBlock.RootHash(spendLeaf.Script)
	tapKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash)
	if tapKey.SerializeCompressed()[0] ==
		secp256k1.PubKeyFormatCompressedOdd {

		controlBlock.OutputKeyYIsOdd = true
	}

	return controlBlock, nil
}

// GenTaprootAssetRootFromProof verifies the inclusion, exclusion, and optional
// split-root proofs before returning the committed Taproot Asset root.
func GenTaprootAssetRootFromProof(assetProof *proof.Proof) ([]byte, error) {
	if assetProof == nil {
		return nil, fmt.Errorf("asset proof is required")
	}

	assetCommitment, err := assetProof.VerifyProofs()
	if err != nil {
		return nil, fmt.Errorf("invalid asset proof: %w", err)
	}

	taprootAssetRoot := assetCommitment.TapscriptRoot(nil)

	return taprootAssetRoot[:], nil
}

// GetPkScriptFromProof returns the top-level Bitcoin script that commits to
// the verified proof's exact Taproot Asset commitment under this kit's HTLC
// tree. The proof is required because an asset alone does not identify its
// anchor commitment version or any co-anchored leaves.
func (s *SwapKit) GetPkScriptFromProof(assetProof *proof.Proof) (
	[]byte, error) {

	if assetProof == nil {
		return nil, fmt.Errorf("asset proof is required")
	}
	if err := s.validateAsset(&assetProof.Asset); err != nil {
		return nil, err
	}

	taprootAssetRoot, err := GenTaprootAssetRootFromProof(assetProof)
	if err != nil {
		return nil, err
	}

	return s.GetPkScriptFromRoot(taprootAssetRoot)
}

func (s *SwapKit) validateAsset(htlcAsset *asset.Asset) error {
	if err := s.validateScripts(); err != nil {
		return err
	}
	if s.amount == 0 {
		return fmt.Errorf("asset amount must be positive")
	}
	if htlcAsset == nil {
		return fmt.Errorf("asset is required")
	}

	proofID := htlcAsset.Genesis.ID()
	if !bytes.Equal(s.assetID[:], proofID[:]) {
		return fmt.Errorf("asset proof ID does not match swap")
	}
	if htlcAsset.Amount != s.amount {
		return fmt.Errorf("asset proof amount does not match swap")
	}
	if htlcAsset.Version != asset.V0 &&
		htlcAsset.Version != asset.V1 {

		return fmt.Errorf("unsupported legacy deposit asset version")
	}
	if htlcAsset.ScriptVersion != asset.ScriptV0 {
		return fmt.Errorf("asset proof script version does not match swap")
	}
	if htlcAsset.LockTime != 0 || htlcAsset.RelativeLockTime != 0 {
		return fmt.Errorf("asset proof contains an unexpected locktime")
	}

	expectedScriptKey, _, _, _, err := CreateOpTrueLeaf()
	if err != nil {
		return err
	}
	expectedScriptKey = asset.NewScriptKey(expectedScriptKey.PubKey)
	if htlcAsset.ScriptKey.PubKey == nil ||
		!htlcAsset.ScriptKey.PubKey.IsEqual(expectedScriptKey.PubKey) {

		return fmt.Errorf("asset proof script key does not match swap")
	}

	return nil
}

type validatedSweep struct {
	assetInputIndex int
	prevOutputs     []*wire.TxOut
	assetRoot       []byte
}

// SpendWitness binds a witness stack to the exact transaction input selected
// by the proof outpoint.
type SpendWitness struct {
	InputIndex uint32
	Witness    wire.TxWitness
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

func (s *SwapKit) validateSweep(assetProof *proof.Proof,
	sweepPacket *psbt.Packet) (*validatedSweep, error) {

	if assetProof == nil {
		return nil, fmt.Errorf("asset proof is required")
	}
	if err := s.validateAsset(&assetProof.Asset); err != nil {
		return nil, err
	}
	assetCommitment, err := assetProof.VerifyProofs()
	if err != nil {
		return nil, fmt.Errorf("invalid asset proof: %w", err)
	}
	assetRootHash := assetCommitment.TapscriptRoot(nil)
	assetRoot := append([]byte(nil), assetRootHash[:]...)
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

	proofOutpoint := assetProof.OutPoint()
	assetInputIndex, err := findUniqueInput(
		sweepPacket.UnsignedTx, proofOutpoint,
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
			Value:    witnessUtxo.Value,
			PkScript: append([]byte(nil), witnessUtxo.PkScript...),
		}
	}
	siblingPreimage, err := s.GetSiblingPreimage()
	if err != nil {
		return nil, err
	}
	siblingHash, err := siblingPreimage.TapHash()
	if err != nil {
		return nil, err
	}
	internalKey, err := s.GetAggregateKey()
	if err != nil {
		return nil, err
	}
	expectedPkScript, err := tapscript.PayToAddrScript(
		*internalKey, siblingHash, *assetCommitment,
	)
	if err != nil {
		return nil, err
	}
	proofOutputIndex := int(proofOutpoint.Index)
	if proofOutputIndex >= len(assetProof.AnchorTx.TxOut) {
		return nil, fmt.Errorf("proof output index is out of bounds")
	}
	proofTxOut := assetProof.AnchorTx.TxOut[proofOutputIndex]
	if proofTxOut == nil {
		return nil, fmt.Errorf("proof anchor output is nil")
	}
	assetPrevOut := prevOutputs[assetInputIndex]
	if proofTxOut.Value != assetPrevOut.Value ||
		!bytes.Equal(proofTxOut.PkScript, assetPrevOut.PkScript) {

		return nil, fmt.Errorf("asset prevout does not match proof anchor")
	}
	if !bytes.Equal(expectedPkScript, assetPrevOut.PkScript) {
		return nil, fmt.Errorf("asset input script does not match proof")
	}

	return &validatedSweep{
		assetInputIndex: assetInputIndex,
		prevOutputs:     prevOutputs,
		assetRoot:       assetRoot,
	}, nil
}

// AssetInputIndex returns the unique input that spends the proof outpoint. It
// validates all PSBT prevouts and the proof-bound anchor output before exposing
// the index. Callers use this index to set the required relative sequence
// before asking the kit to sign.
func (s *SwapKit) AssetInputIndex(assetProof *proof.Proof,
	sweepPacket *psbt.Packet) (uint32, error) {

	sweep, err := s.validateSweep(assetProof, sweepPacket)
	if err != nil {
		return 0, err
	}

	return uint32(sweep.assetInputIndex), nil
}

func validateSequence(sweep *validatedSweep, sweepPacket *psbt.Packet,
	required uint32) error {

	sequence := sweepPacket.UnsignedTx.TxIn[sweep.assetInputIndex].Sequence
	if sequence != required {
		return fmt.Errorf(
			"asset input sequence must be %d, got %d",
			required, sequence,
		)
	}

	return nil
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

// CreatePreimageWitness signs the exact proof-bound asset input and returns a
// success-path witness in the legacy stack order.
func (s *SwapKit) CreatePreimageWitness(ctx context.Context,
	signer lndclient.SignerClient, htlcProof *proof.Proof,
	sweepBtcPacket *psbt.Packet, keyLocator keychain.KeyLocator,
	preimage lntypes.Preimage) (*SpendWitness, error) {

	if signer == nil {
		return nil, fmt.Errorf("signer is required")
	}
	if preimage.Hash() != s.swapHash {
		return nil, fmt.Errorf("preimage does not match swap hash")
	}

	sweep, err := s.validateSweep(htlcProof, sweepBtcPacket)
	if err != nil {
		return nil, err
	}
	if err := validateSequence(
		sweep, sweepBtcPacket, SuccessSequence,
	); err != nil {
		return nil, err
	}

	successScript, err := s.GetSuccessScript()
	if err != nil {
		return nil, err
	}

	signDesc := &lndclient.SignDescriptor{
		KeyDesc:       keychain.KeyDescriptor{KeyLocator: keyLocator},
		SignMethod:    input.TaprootScriptSpendSignMethod,
		WitnessScript: successScript,
		Output:        sweep.prevOutputs[sweep.assetInputIndex],
		InputIndex:    sweep.assetInputIndex,
	}
	sigs, err := signer.SignOutputRaw(
		ctx, sweepBtcPacket.UnsignedTx,
		[]*lndclient.SignDescriptor{signDesc}, sweep.prevOutputs,
	)
	if err != nil {
		return nil, err
	}
	if len(sigs) != 1 || len(sigs[0]) == 0 {
		return nil, fmt.Errorf("signer returned an invalid signature set")
	}
	if err := verifyTapscriptSignature(
		sweepBtcPacket.UnsignedTx, sweep, successScript,
		s.receiverPubKey, sigs[0],
	); err != nil {
		return nil, err
	}

	successControlBlock, err := s.GenSuccessBtcControlBlock(
		sweep.assetRoot,
	)
	if err != nil {
		return nil, err
	}
	controlBlockBytes, err := successControlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return &SpendWitness{
		InputIndex: uint32(sweep.assetInputIndex),
		Witness: wire.TxWitness{
			preimage[:], sigs[0], successScript, controlBlockBytes,
		},
	}, nil
}

// CreateTimeoutWitness signs the exact proof-bound asset input and returns a
// timeout-path witness in the legacy stack order.
func (s *SwapKit) CreateTimeoutWitness(ctx context.Context,
	signer lndclient.SignerClient, htlcProof *proof.Proof,
	sweepBtcPacket *psbt.Packet, keyLocator keychain.KeyLocator) (
	*SpendWitness, error) {

	if signer == nil {
		return nil, fmt.Errorf("signer is required")
	}

	sweep, err := s.validateSweep(htlcProof, sweepBtcPacket)
	if err != nil {
		return nil, err
	}
	if err := validateSequence(
		sweep, sweepBtcPacket, s.csvExpiry,
	); err != nil {
		return nil, err
	}

	timeoutScript, err := s.GetTimeoutScript()
	if err != nil {
		return nil, err
	}

	signDesc := &lndclient.SignDescriptor{
		KeyDesc:       keychain.KeyDescriptor{KeyLocator: keyLocator},
		SignMethod:    input.TaprootScriptSpendSignMethod,
		WitnessScript: timeoutScript,
		Output:        sweep.prevOutputs[sweep.assetInputIndex],
		InputIndex:    sweep.assetInputIndex,
	}
	sigs, err := signer.SignOutputRaw(
		ctx, sweepBtcPacket.UnsignedTx,
		[]*lndclient.SignDescriptor{signDesc}, sweep.prevOutputs,
	)
	if err != nil {
		return nil, err
	}
	if len(sigs) != 1 || len(sigs[0]) == 0 {
		return nil, fmt.Errorf("signer returned an invalid signature set")
	}
	if err := verifyTapscriptSignature(
		sweepBtcPacket.UnsignedTx, sweep, timeoutScript,
		s.senderPubKey, sigs[0],
	); err != nil {
		return nil, err
	}

	timeoutControlBlock, err := s.GenTimeoutBtcControlBlock(
		sweep.assetRoot,
	)
	if err != nil {
		return nil, err
	}
	controlBlockBytes, err := timeoutControlBlock.ToBytes()
	if err != nil {
		return nil, err
	}

	return &SpendWitness{
		InputIndex: uint32(sweep.assetInputIndex),
		Witness: wire.TxWitness{
			sigs[0], timeoutScript, controlBlockBytes,
		},
	}, nil
}
