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
	"github.com/lightninglabs/loop/assets/internal/policy"
	assetsweep "github.com/lightninglabs/loop/assets/sweep"
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
	policy.TapdInfoClient

	NewAddr(context.Context, *taprpc.NewAddrRequest,
		...grpc.CallOption) (*taprpc.Addr, error)
	ExportProof(context.Context, *taprpc.ExportProofRequest,
		...grpc.CallOption) (*taprpc.ProofFile, error)
}

// ProofVerifier is the narrow tapd boundary needed to verify a complete proof
// file against tapd's chain backend and asset transition verifier.
type ProofVerifier interface {
	policy.TapdInfoClient

	VerifyProof(context.Context, *taprpc.ProofFile,
		...grpc.CallOption) (*taprpc.VerifyProofResponse, error)
}

var _ ProofVerifier = (taprpc.TaprootAssetsClient)(nil)

// Params defines a deposit contract. Sender and receiver follow the on-chain
// asset direction, independently of which party is the client or server.
type Params struct {
	SenderInternalPubKey   *btcec.PublicKey
	ReceiverInternalPubKey *btcec.PublicKey
	SenderScriptKey        *btcec.PublicKey
	ReceiverScriptKey      *btcec.PublicKey

	// KeyLocator identifies the sender script key used for refund signing.
	KeyLocator  keychain.KeyLocator
	AssetID     asset.ID
	CsvExpiry   uint32
	ChainParams *address.ChainParams

	// Genesis and GroupKey are trusted asset metadata, obtained from a
	// verified issuance/history. Genesis must match AssetID and is required
	// for address creation and validation. A nil GroupKey means ungrouped.
	// When Genesis is supplied, proof validation also enforces this group
	// identity. Proof-only kits may omit both fields.
	Genesis  *asset.Genesis
	GroupKey *btcec.PublicKey
}

// Kit contains the immutable information needed to create and operate a
// two-party MuSig2 asset deposit.
type Kit struct {
	senderInternalPubKey   *btcec.PublicKey
	receiverInternalPubKey *btcec.PublicKey
	senderScriptKey        *btcec.PublicKey
	receiverScriptKey      *btcec.PublicKey
	keyLocator             keychain.KeyLocator
	assetID                asset.ID
	csvExpiry              uint32
	muSig2Key              *musig2.AggregateKey
	chainParams            *address.ChainParams
	genesis                *asset.Genesis
	groupKey               *btcec.PublicKey
}

// NewKit creates a validated legacy asset deposit kit. Internal keys form the
// cooperative aggregate; script keys authorize unilateral deposit/HTLC spends.
// A party may reuse its own internal key as its script key for legacy contracts.
func NewKit(params Params) (*Kit, error) {
	if params.AssetID == (asset.ID{}) {
		return nil, fmt.Errorf("asset ID is required")
	}
	if params.CsvExpiry == 0 {
		return nil, fmt.Errorf("CSV expiry must be positive")
	}
	if params.CsvExpiry > wire.SequenceLockTimeMask {
		return nil, fmt.Errorf("CSV expiry exceeds block-based BIP68 range")
	}

	// The network name selects the proof activation height, so it must
	// come from the canonical parameters rather than from the caller.
	chainParams, err := policy.CanonicalAddressParams(params.ChainParams)
	if err != nil {
		return nil, err
	}

	var genesis *asset.Genesis
	if params.Genesis != nil {
		copyGenesis := *params.Genesis
		if copyGenesis.ID() != params.AssetID {
			return nil, fmt.Errorf("genesis does not match asset ID")
		}
		if copyGenesis.FirstPrevOut == (wire.OutPoint{}) ||
			(copyGenesis.Type != asset.Normal &&
				copyGenesis.Type != asset.Collectible) {

			return nil, fmt.Errorf("invalid address asset genesis")
		}
		genesis = &copyGenesis
	}
	var groupKey *btcec.PublicKey
	if params.GroupKey != nil {
		if genesis == nil {
			return nil, fmt.Errorf("group key requires asset genesis")
		}
		groupKey, err = clonePublicKey(params.GroupKey, "asset group")
		if err != nil {
			return nil, err
		}
	}

	senderInternalKey, err := clonePublicKey(
		params.SenderInternalPubKey, "sender internal",
	)
	if err != nil {
		return nil, err
	}
	receiverInternalKey, err := clonePublicKey(
		params.ReceiverInternalPubKey, "receiver internal",
	)
	if err != nil {
		return nil, err
	}
	senderScriptKey, err := clonePublicKey(
		params.SenderScriptKey, "sender script",
	)
	if err != nil {
		return nil, err
	}
	receiverScriptKey, err := clonePublicKey(
		params.ReceiverScriptKey, "receiver script",
	)
	if err != nil {
		return nil, err
	}
	senderKeys := []*btcec.PublicKey{senderInternalKey, senderScriptKey}
	receiverKeys := []*btcec.PublicKey{receiverInternalKey, receiverScriptKey}
	for _, senderKey := range senderKeys {
		for _, receiverKey := range receiverKeys {
			if bytes.Equal(schnorr.SerializePubKey(senderKey),
				schnorr.SerializePubKey(receiverKey)) {

				return nil, fmt.Errorf("sender and receiver keys " +
					"must differ across roles")
			}
		}
	}

	sortKeys := true
	muSig2Key, err := input.MuSig2CombineKeys(
		input.MuSig2Version100RC2,
		[]*btcec.PublicKey{senderInternalKey, receiverInternalKey}, sortKeys,
		// The actual BIP341 tweak includes the asset and timeout roots,
		// which are only known once the deposit proof is available.
		&input.MuSig2Tweaks{},
	)
	if err != nil {
		return nil, err
	}

	return &Kit{
		senderInternalPubKey:   senderInternalKey,
		receiverInternalPubKey: receiverInternalKey,
		senderScriptKey:        senderScriptKey,
		receiverScriptKey:      receiverScriptKey,
		keyLocator:             params.KeyLocator,
		assetID:                params.AssetID,
		csvExpiry:              params.CsvExpiry,
		muSig2Key:              muSig2Key,
		chainParams:            chainParams,
		genesis:                genesis,
		groupKey:               groupKey,
	}, nil
}

// clonePublicKey validates a named key role and returns an independent copy.
func clonePublicKey(key *btcec.PublicKey, role string) (*btcec.PublicKey, error) {
	if key == nil {
		return nil, fmt.Errorf("%s public key is required", role)
	}
	keyCopy, err := btcec.ParsePubKey(key.SerializeCompressed())
	if err != nil {
		return nil, fmt.Errorf("invalid %s public key: %w", role, err)
	}

	return keyCopy, nil
}

// GenTimeoutPathScript constructs the deposit funder's CSV timeout script.
//
//	<senderScriptKey> OP_CHECKSIGVERIFY <csvExpiry> OP_CHECKSEQUENCEVERIFY
func (d *Kit) GenTimeoutPathScript() ([]byte, error) {
	if d == nil || d.senderScriptKey == nil {
		return nil, fmt.Errorf("deposit kit is incomplete")
	}

	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(d.senderScriptKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddInt64(int64(d.csvExpiry))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	return builder.Script()
}

// timeoutPathSibling returns the timeout leaf as a Taproot Asset sibling
// preimage.
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

// encodedTimeoutPathSibling returns the timeout sibling in its canonical wire
// encoding.
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
// timeout path. New addresses use asset V1 and address V1 (commitment V2), as
// does the HTLC funding template; legacy proofs remain accepted when spending.
// Trusted Genesis and GroupKey metadata must be supplied to NewKit. The RPC
// response is validated against the contract and returned as an owned copy.
// The connected tapd must satisfy the shared on-chain swap version floor.
func (d *Kit) NewAddr(ctx context.Context, client AddressProofClient,
	amount uint64) (*taprpc.Addr, error) {

	if d == nil {
		return nil, fmt.Errorf("deposit kit is required")
	}
	if client == nil {
		return nil, fmt.Errorf("tapd client is required")
	}
	if err := d.validateAddressAsset(amount); err != nil {
		return nil, err
	}

	siblingBytes, err := d.encodedTimeoutPathSibling()
	if err != nil {
		return nil, err
	}
	tapScriptKey, _, _, _, err := htlc.CreateOpTrueLeaf()
	if err != nil {
		return nil, err
	}

	if err := policy.RequireOnchainSwapTapd(ctx, client); err != nil {
		return nil, err
	}

	addr, err := client.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId:   bytes.Clone(d.assetID[:]),
		Amt:       amount,
		ScriptKey: rpcutils.MarshalScriptKey(tapScriptKey),
		InternalKey: &taprpc.KeyDescriptor{
			RawKeyBytes: d.muSig2Key.PreTweakedKey.SerializeCompressed(),
		},
		TapscriptSibling: bytes.Clone(siblingBytes),
		AssetVersion:     policy.RPCAssetVersion,
		AddressVersion:   policy.RPCAddressVersion,
	})
	if err != nil {
		return nil, err
	}
	return d.validateAddrResponse(
		addr, amount, d.muSig2Key.PreTweakedKey, siblingBytes,
	)
}

// NewHtlcAddr creates an HTLC address using the deposit parties as sender and
// receiver under AssetHTLCV0. The returned kit's non-interactive CreateHtlcVpkt
// template funds this predictive V1 address for exact-value and change sends.
// Asset and address versions are pinned rather than relying on RPC defaults.
// Trusted Genesis and GroupKey metadata must be supplied to NewKit. Both the
// encoded address and expanded RPC fields are validated before returning.
// The connected tapd must satisfy the shared on-chain swap version floor.
func (d *Kit) NewHtlcAddr(ctx context.Context, client AddressProofClient,
	amount uint64, swapHash lntypes.Hash, csvExpiry uint32) (
	*taprpc.Addr, *htlc.SwapKit, error) {

	if d == nil {
		return nil, nil, fmt.Errorf("deposit kit is required")
	}
	if client == nil {
		return nil, nil, fmt.Errorf("tapd client is required")
	}
	if err := d.validateAddressAsset(amount); err != nil {
		return nil, nil, err
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

	if err := policy.RequireOnchainSwapTapd(ctx, client); err != nil {
		return nil, nil, err
	}

	htlcAddr, err := client.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId:   bytes.Clone(d.assetID[:]),
		Amt:       amount,
		ScriptKey: rpcutils.MarshalScriptKey(tapScriptKey),
		InternalKey: &taprpc.KeyDescriptor{
			RawKeyBytes: btcInternalKey.SerializeCompressed(),
		},
		TapscriptSibling: bytes.Clone(siblingBytes),
		AssetVersion:     policy.RPCAssetVersion,
		AddressVersion:   policy.RPCAddressVersion,
	})
	if err != nil {
		return nil, nil, err
	}

	validated, err := d.validateAddrResponse(
		htlcAddr, amount, btcInternalKey, siblingBytes,
	)
	if err != nil {
		return nil, nil, err
	}
	return validated, swapKit, nil
}

// newHtlcSwapKit derives the immutable legacy HTLC contract for a deposit.
func (d *Kit) newHtlcSwapKit(amount uint64, swapHash lntypes.Hash,
	csvExpiry uint32) (*htlc.SwapKit, error) {

	return htlc.NewSwapKit(htlc.AssetHTLCV0, htlc.Params{
		SenderInternalPubKey:   d.senderInternalPubKey,
		ReceiverInternalPubKey: d.receiverInternalPubKey,
		SenderScriptKey:        d.senderScriptKey,
		ReceiverScriptKey:      d.receiverScriptKey,
		AssetID:                d.assetID,
		Amount:                 amount,
		SwapHash:               swapHash,
		CsvExpiry:              csvExpiry,
		AddressParams:          d.chainParams,
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
		AssetId:   bytes.Clone(d.assetID[:]),
		ScriptKey: scriptKey.PubKey.SerializeCompressed(),
		Outpoint: &taprpc.OutPoint{
			Txid:        outpoint.Hash[:],
			OutputIndex: outpoint.Index,
		},
	})
}

// validateAsset binds an asset leaf to the identity and script policy of this
// deposit kit, including the expected group when trusted metadata is supplied.
// Exact amounts are checked by the operation that knows the deposit amount.
func (d *Kit) validateAsset(depositAsset *asset.Asset) error {
	if depositAsset == nil {
		return fmt.Errorf("deposit asset is required")
	}
	proofID := depositAsset.Genesis.ID()
	if !bytes.Equal(d.assetID[:], proofID[:]) {
		return fmt.Errorf("asset proof ID does not match deposit")
	}
	if d.genesis != nil {
		var proofGroupKey *btcec.PublicKey
		if depositAsset.GroupKey != nil {
			proofGroupKey = &depositAsset.GroupKey.GroupPubKey
		}
		if !equalOptionalKey(proofGroupKey, d.groupKey) {
			return fmt.Errorf("asset proof group key does not match deposit")
		}
	}
	if depositAsset.Amount == 0 {
		return fmt.Errorf("deposit asset amount must be positive")
	}
	if !policy.SupportsAssetVersion(depositAsset.Version) {
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

// validateProof verifies the proof's commitment structure and binds its
// internal key and timeout sibling to this deposit kit. It allows co-anchored
// leaves, which a signer may supply in its transfer; acceptance additionally
// requires the proof to describe the complete anchor commitment.
func (d *Kit) validateProof(depositProof *proof.Proof) (
	*commitment.TapCommitment, error) {

	if d == nil {
		return nil, fmt.Errorf("deposit kit is required")
	}
	if depositProof == nil {
		return nil, fmt.Errorf("deposit proof is required")
	}
	if err := policy.ValidateTransitionVersion(depositProof.Version); err != nil {
		return nil, err
	}
	if err := d.validateAsset(&depositProof.Asset); err != nil {
		return nil, err
	}

	tapCommitment, err := depositProof.VerifyProofs()
	if err != nil {
		return nil, fmt.Errorf("invalid deposit proof: %w", err)
	}
	if err := policy.ValidateCommitmentVersion(tapCommitment.Version); err != nil {
		return nil, err
	}
	// The anchor commits to the x-only internal key, so a proof may carry
	// either parity of the aggregate key.
	internalKey := depositProof.InclusionProof.InternalKey
	if internalKey == nil || !bytes.Equal(
		schnorr.SerializePubKey(internalKey),
		schnorr.SerializePubKey(d.muSig2Key.PreTweakedKey),
	) {

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

// anchorRootFromProofCommitment verifies the proof's commitment structure,
// requires it to describe the complete anchor commitment, and returns the
// complete anchor Taproot Merkle root.
func (d *Kit) anchorRootFromProofCommitment(depositProof *proof.Proof) (
	[]byte, error) {

	tapCommitment, err := d.validateProof(depositProof)
	if err != nil {
		return nil, err
	}

	// Every spend of the deposit must rebuild its complete input
	// commitment, so a funder could otherwise hide a leaf to block the
	// cooperative spends and HTLC conversions that protect the receiver.
	if _, err := htlc.VerifyCompleteAnchor(depositProof); err != nil {
		return nil, fmt.Errorf("incomplete deposit proof: %w", err)
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

// AnchorRootFromProofCommitment verifies the proof's commitment structure,
// binds its asset ID, script key, internal key, timeout sibling, and exact
// amount to this deposit, then returns the complete anchor Taproot Merkle root.
// Trusted group metadata is enforced when supplied to the kit.
// The proof must describe the complete anchor commitment, so anchors with
// co-anchored leaves missing from the proof are rejected.
// It does not verify the proof's chain inclusion, provenance, state transition,
// confirmation depth, or whether the anchor outpoint remains unspent. Use
// VerifyProofFile when a proof file has not already been fully verified.
func (d *Kit) AnchorRootFromProofCommitment(depositProof *proof.Proof,
	expectedAmount uint64) ([]byte, error) {

	if d == nil {
		return nil, fmt.Errorf("deposit kit is required")
	}
	if expectedAmount == 0 {
		return nil, fmt.Errorf("expected deposit amount must be positive")
	}
	if depositProof == nil {
		return nil, fmt.Errorf("deposit proof is required")
	}
	if depositProof.Asset.Amount != expectedAmount {
		return nil, fmt.Errorf(
			"deposit asset amount mismatch: expected %d, got %d",
			expectedAmount, depositProof.Asset.Amount,
		)
	}

	return d.anchorRootFromProofCommitment(depositProof)
}

// VerifyProofFile requires genesis-rooted histories without ownership
// challenges, including all additional inputs, and the network's Taproot
// Assets v0.8.5 activation rules for every proof, then asks tapd to fully
// verify the proof file. It binds the terminal proof to the expected deposit
// outpoint and amount, requires it to describe the complete anchor commitment,
// and returns the complete anchor Taproot Merkle root. The caller must still
// enforce confirmation depth and check that the outpoint is unspent. Even
// when re-verifying an existing swap, untrusted history acceptance requires a
// tapd that satisfies the shared version floor. Local refund signing remains
// available with a previously verified proof.
func (d *Kit) VerifyProofFile(ctx context.Context, verifier ProofVerifier,
	proofFile *taprpc.ProofFile, expectedOutpoint *wire.OutPoint,
	expectedAmount uint64) ([]byte, error) {

	if d == nil {
		return nil, fmt.Errorf("deposit kit is required")
	}
	if verifier == nil {
		return nil, fmt.Errorf("proof verifier is required")
	}
	if proofFile == nil || len(proofFile.RawProofFile) == 0 {
		return nil, fmt.Errorf("proof file is required")
	}
	if expectedOutpoint == nil {
		return nil, fmt.Errorf("expected deposit outpoint is required")
	}
	if expectedAmount == 0 {
		return nil, fmt.Errorf("expected deposit amount must be positive")
	}

	rawProof := proofFile.RawProofFile
	if err := policy.CheckProofFileSize(rawProof); err != nil {
		return nil, fmt.Errorf("invalid deposit proof file: %w", err)
	}
	if err := policy.RequireOnchainSwapTapd(ctx, verifier); err != nil {
		return nil, err
	}

	proofFileCopy := &taprpc.ProofFile{
		RawProofFile: bytes.Clone(rawProof),
		GenesisPoint: proofFile.GenesisPoint,
	}
	decodedFile, err := proof.DecodeFile(proofFileCopy.RawProofFile)
	if err != nil {
		return nil, fmt.Errorf("unable to decode deposit proof file: %w", err)
	}
	err = policy.ValidateProvenance(ctx, decodedFile, d.chainParams)
	if err != nil {
		return nil, fmt.Errorf("invalid deposit proof history: %w", err)
	}
	verifyResponse, err := verifier.VerifyProof(ctx, proofFileCopy)
	if err != nil {
		return nil, fmt.Errorf("unable to verify deposit proof file: %w", err)
	}
	if verifyResponse == nil || !verifyResponse.Valid {
		return nil, fmt.Errorf("invalid deposit proof file")
	}

	depositProof, err := decodedFile.LastProof()
	if err != nil {
		return nil, fmt.Errorf("unable to read terminal deposit proof: %w", err)
	}
	if depositProof.OutPoint() != *expectedOutpoint {
		return nil, fmt.Errorf(
			"deposit proof outpoint mismatch: expected %v, got %v",
			*expectedOutpoint, depositProof.OutPoint(),
		)
	}

	return d.AnchorRootFromProofCommitment(depositProof, expectedAmount)
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
	internalKey := *d.muSig2Key.PreTweakedKey
	controlBlock := &txscript.ControlBlock{
		InternalKey:    &internalKey,
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

// findUniqueInput returns the sole transaction input that spends outpoint.
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

// validateSweep binds a PSBT input and witness UTXO to the deposit proof and
// reconstructs the exact Taproot output being spent.
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

// verifyTapscriptSignature checks a signer response against the exact input,
// prevouts, script, and public key used by the timeout spend.
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

// AssetInputIndex returns the unique proof-bound deposit input after validating
// the proof and PSBT prevouts. Callers must set this input's sequence to the
// deposit's CSV expiry before signing any transaction inputs.
func (d *Kit) AssetInputIndex(depositProof *proof.Proof,
	sweepPacket *psbt.Packet) (uint32, error) {

	sweep, err := d.validateSweep(depositProof, sweepPacket)
	if err != nil {
		return 0, err
	}

	return uint32(sweep.assetInputIndex), nil
}

// CreateTimeoutWitness signs the exact proof-bound deposit input and returns
// its input index and timeout-path witness. The caller must set the deposit
// input's sequence to the CSV expiry before calling this method. The PSBT is
// never mutated here, preserving signatures on other inputs. The transfer
// must contain the caller-approved, complete active and passive asset packets.
func (d *Kit) CreateTimeoutWitness(ctx context.Context,
	signer lndclient.SignerClient, depositProof *proof.Proof,
	sweepPacket *psbt.Packet, transfer *assetsweep.Transfer) (
	*htlc.SpendWitness, error) {

	if signer == nil {
		return nil, fmt.Errorf("signer is required")
	}
	sweep, err := d.validateSweep(depositProof, sweepPacket)
	if err != nil {
		return nil, err
	}
	sequence := sweepPacket.UnsignedTx.TxIn[sweep.assetInputIndex].Sequence
	if sequence != d.csvExpiry {
		return nil, fmt.Errorf("asset input sequence must be %d, got %d",
			d.csvExpiry, sequence)
	}
	if err := transfer.Validate(depositProof, sweepPacket); err != nil {
		return nil, err
	}
	sweepTx := sweepPacket.UnsignedTx
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
		sweepTx, sweep, timeoutScript, d.senderScriptKey,
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
	return &htlc.SpendWitness{
		InputIndex: uint32(sweep.assetInputIndex),
		Witness: wire.TxWitness{
			sigs[0], timeoutScript, controlBlockBytes,
		},
	}, nil
}
