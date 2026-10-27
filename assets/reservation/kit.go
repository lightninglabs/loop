package reservation

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/loop/internal/assetproof"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/loop/swapkit/tapsdk"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/grpc"
)

// AddressProofClient creates reservation addresses and exports their proofs.
type AddressProofClient interface {
	NewAddr(context.Context, *taprpc.NewAddrRequest,
		...grpc.CallOption) (*taprpc.Addr, error)
	ExportProof(context.Context, *taprpc.ExportProofRequest,
		...grpc.CallOption) (*taprpc.ProofFile, error)
}

// ProofVerifier checks the running tapd and verifies a complete proof file.
type ProofVerifier interface {
	assetpolicy.TapdInfoClient
	VerifyProof(context.Context, *taprpc.ProofFile,
		...grpc.CallOption) (*taprpc.VerifyProofResponse, error)
}

// Kit adapts the shared CSV deposit contract to server-funded reservations.
// The server is the sender and owns the unilateral refund key.
type Kit struct {
	contract  *swapkit.DepositKit
	serverKey *btcec.PublicKey
	locator   keychain.KeyLocator
	assetID   asset.ID
	params    *address.ChainParams
	csvDelay  uint32
}

// NewKit constructs a server-funded CSV reservation. Explicit key roles put
// the server on the timeout path.
func NewKit(server, client *btcec.PublicKey, locator keychain.KeyLocator,
	assetID asset.ID, csvDelay uint32,
	params *address.ChainParams) (*Kit, error) {

	if server == nil || client == nil || assetID == asset.ZeroID ||
		params == nil || params.Params == nil || params.TapHRP == "" {

		return nil, errors.New("invalid reservation contract")
	}
	serverCopy, err := btcec.ParsePubKey(server.SerializeCompressed())
	if err != nil {
		return nil, err
	}
	serverPub := swapkit.PubKey(server.SerializeCompressed())
	clientPub := swapkit.PubKey(client.SerializeCompressed())
	contract, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		SenderInternalKey:   serverPub,
		ReceiverInternalKey: clientPub,
		SenderScriptKey:     serverPub,
		ReceiverScriptKey:   clientPub,
		CSVExpiry:           csvDelay,
	})
	if err != nil {
		return nil, err
	}
	return &Kit{
		contract: contract, serverKey: serverCopy, locator: locator,
		assetID: assetID, params: assetpolicy.CloneAddressParams(*params),
		csvDelay: csvDelay,
	}, nil
}

// GenTimeoutPathScript returns the server's CSV refund leaf.
func (k *Kit) GenTimeoutPathScript() ([]byte, error) {
	if k == nil || k.contract == nil {
		return nil, errors.New("reservation contract is required")
	}
	return k.contract.TimeoutScript(), nil
}

func (k *Kit) sibling() (*commitment.TapscriptPreimage, error) {
	script, err := k.GenTimeoutPathScript()
	if err != nil {
		return nil, err
	}
	return commitment.NewPreimageFromLeaf(txscript.NewBaseTapLeaf(script))
}

// TapScriptKey returns the x-only NUMS/OP_TRUE key used in asset proofs.
func (k *Kit) TapScriptKey() (asset.ScriptKey, error) {
	key, err := k.tapAddressScriptKey()
	if err != nil {
		return asset.ScriptKey{}, err
	}
	return asset.NewScriptKey(key.PubKey), nil
}

// tapAddressScriptKey retains the tweaked key's parity and derivation for
// tapd's address request. Asset proofs serialize only its x coordinate.
func (k *Kit) tapAddressScriptKey() (asset.ScriptKey, error) {
	if k == nil || k.contract == nil {
		return asset.ScriptKey{}, errors.New("reservation contract is required")
	}
	descriptor, _, err := tapsdk.LegacyOPTrueDescriptor()
	if err != nil {
		return asset.ScriptKey{}, err
	}
	// SDK keys use an even x-only representative. Tapd's address request
	// retains the tweaked output key's parity; proofs use only its x value.
	pub := txscript.ComputeTaprootOutputKey(
		asset.NUMSPubKey, descriptor.TapTweak,
	)
	return asset.ScriptKey{
		PubKey: pub,
		TweakedScriptKey: &asset.TweakedScriptKey{
			RawKey: keychain.KeyDescriptor{PubKey: asset.NUMSPubKey},
			Tweak:  bytes.Clone(descriptor.TapTweak),
		},
	}, nil
}

// NewAddr creates a V1 reservation address with its CSV sibling.
func (k *Kit) NewAddr(ctx context.Context, tap AddressProofClient,
	amount uint64) (*taprpc.Addr, error) {

	if tap == nil || amount == 0 {
		return nil, errors.New("invalid reservation address request")
	}
	scriptKey, err := k.tapAddressScriptKey()
	if err != nil {
		return nil, err
	}
	sibling, err := k.sibling()
	if err != nil {
		return nil, err
	}
	raw, _, err := commitment.MaybeEncodeTapscriptPreimage(sibling)
	if err != nil {
		return nil, err
	}
	plan := k.contract.AnchorPlan()
	return tap.NewAddr(ctx, &taprpc.NewAddrRequest{
		AssetId: bytes.Clone(k.assetID[:]), Amt: amount,
		ScriptKey: rpcutils.MarshalScriptKey(scriptKey),
		InternalKey: &taprpc.KeyDescriptor{
			RawKeyBytes: bytes.Clone(plan.InternalKey[:]),
		},
		TapscriptSibling: raw,
		AssetVersion:     taprpc.AssetVersion_ASSET_VERSION_V1,
		AddressVersion:   taprpc.AddrVersion_ADDR_VERSION_V1,
	})
}

// ExportProof requests the proof for one exact reservation output.
func (k *Kit) ExportProof(ctx context.Context, tap AddressProofClient,
	point *wire.OutPoint) (*taprpc.ProofFile, error) {

	if k == nil || tap == nil || point == nil {
		return nil, errors.New("invalid reservation proof request")
	}
	scriptKey, err := k.TapScriptKey()
	if err != nil {
		return nil, err
	}
	return tap.ExportProof(ctx, &taprpc.ExportProofRequest{
		AssetId:   bytes.Clone(k.assetID[:]),
		ScriptKey: scriptKey.PubKey.SerializeCompressed(),
		Outpoint: &taprpc.OutPoint{
			Txid: bytes.Clone(point.Hash[:]), OutputIndex: point.Index,
		},
	})
}

func (k *Kit) proofSpend(p *proof.Proof, amount uint64) (
	*swapkit.SwapAnchorSpendInfo, swapkit.Hash, error) {

	if k == nil || k.contract == nil || p == nil || amount == 0 ||
		p.Asset.ID() != k.assetID || p.Asset.Amount != amount ||
		!assetpolicy.SupportsAssetVersion(p.Asset.Version) ||
		p.Asset.ScriptVersion != asset.ScriptV0 ||
		p.Asset.LockTime != 0 || p.Asset.RelativeLockTime != 0 {

		return nil, swapkit.Hash{}, errors.New("asset differs from reservation")
	}
	scriptKey, err := k.TapScriptKey()
	if err != nil || p.Asset.ScriptKey.PubKey == nil ||
		!p.Asset.ScriptKey.PubKey.IsEqual(scriptKey.PubKey) {

		return nil, swapkit.Hash{}, errors.New("invalid reservation script key")
	}
	commitmentRoot, err := assetproof.VerifyCompleteAnchor(p)
	if err != nil {
		return nil, swapkit.Hash{}, err
	}
	plan := k.contract.AnchorPlan()
	if p.InclusionProof.InternalKey == nil || !bytes.Equal(
		p.InclusionProof.InternalKey.SerializeCompressed(),
		plan.InternalKey[:],
	) {

		return nil, swapkit.Hash{}, errors.New("invalid reservation internal key")
	}
	sibling, err := k.sibling()
	if err != nil {
		return nil, swapkit.Hash{}, err
	}
	want, _, err := commitment.MaybeEncodeTapscriptPreimage(sibling)
	if err != nil {
		return nil, swapkit.Hash{}, err
	}
	got, _, err := commitment.MaybeEncodeTapscriptPreimage(
		p.InclusionProof.CommitmentProof.TapSiblingPreimage,
	)
	if err != nil || !bytes.Equal(got, want) {
		return nil, swapkit.Hash{}, errors.New("invalid reservation sibling")
	}
	root := swapkit.Hash(commitmentRoot.TapscriptRoot(nil))
	spend, err := k.contract.TimeoutSpend(root)
	if err != nil {
		return nil, swapkit.Hash{}, err
	}
	index := p.OutPoint().Index
	if index >= uint32(len(p.AnchorTx.TxOut)) ||
		p.AnchorTx.TxOut[index] == nil || !bytes.Equal(
		p.AnchorTx.TxOut[index].PkScript, spend.PkScript,
	) {

		return nil, swapkit.Hash{}, errors.New("invalid reservation anchor")
	}
	return spend, root, nil
}

// AnchorRootFromProofCommitment checks the saved reservation contract and
// returns its complete Bitcoin Taproot root for cooperative signing.
func (k *Kit) AnchorRootFromProofCommitment(p *proof.Proof,
	amount uint64) ([]byte, error) {

	spend, _, err := k.proofSpend(p, amount)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(spend.TaprootMerkleRoot[:]), nil
}

// AssetInputIndex finds the unique proof-bound Bitcoin input in a PSBT.
func (k *Kit) AssetInputIndex(p *proof.Proof,
	packet *psbt.Packet) (uint32, error) {

	if p == nil || packet == nil || packet.UnsignedTx == nil ||
		packet.UnsignedTx.Version < 2 ||
		len(packet.Inputs) != len(packet.UnsignedTx.TxIn) {

		return 0, errors.New("incomplete reservation PSBT")
	}
	if _, _, err := k.proofSpend(p, p.Asset.Amount); err != nil {
		return 0, err
	}
	point := p.OutPoint()
	index := -1
	for i, input := range packet.UnsignedTx.TxIn {
		if input == nil || packet.Inputs[i].WitnessUtxo == nil {
			return 0, errors.New("reservation prevout is missing")
		}
		if input.PreviousOutPoint != point {
			continue
		}
		if index >= 0 {
			return 0, errors.New("reservation input is duplicated")
		}
		index = i
	}
	if index < 0 {
		return 0, errors.New("reservation input is missing")
	}
	expected := p.AnchorTx.TxOut[point.Index]
	got := packet.Inputs[index].WitnessUtxo
	if expected.Value != got.Value ||
		!bytes.Equal(expected.PkScript, got.PkScript) {

		return 0, errors.New("reservation prevout differs from proof")
	}
	return uint32(index), nil
}

// SpendWitness identifies the signed reservation input and its CSV witness.
type SpendWitness struct {
	InputIndex uint32
	Witness    wire.TxWitness
}

// CreateTimeoutWitness signs the server's CSV path after validating the full
// transfer. It leaves the caller's PSBT unchanged.
func (k *Kit) CreateTimeoutWitness(ctx context.Context,
	signer lndclient.SignerClient, p *proof.Proof, packet *psbt.Packet,
	transfer *Transfer) (*SpendWitness, error) {

	if signer == nil || transfer == nil {
		return nil, errors.New("reservation signer and transfer are required")
	}
	index, err := k.AssetInputIndex(p, packet)
	if err != nil {
		return nil, err
	}
	if packet.UnsignedTx.TxIn[index].Sequence != k.csvDelay {
		return nil, errors.New("reservation CSV sequence differs")
	}
	if err := transfer.Validate(p, packet); err != nil {
		return nil, err
	}
	_, assetRoot, err := k.proofSpend(p, p.Asset.Amount)
	if err != nil {
		return nil, err
	}
	spend, err := k.contract.TimeoutSpend(assetRoot)
	if err != nil {
		return nil, err
	}
	prevouts := make([]*wire.TxOut, len(packet.Inputs))
	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, input := range packet.UnsignedTx.TxIn {
		utxo := packet.Inputs[i].WitnessUtxo
		prevouts[i] = &wire.TxOut{
			Value: utxo.Value, PkScript: bytes.Clone(utxo.PkScript),
		}
		fetcher.AddPrevOut(input.PreviousOutPoint, prevouts[i])
	}
	sigs, err := signer.SignOutputRaw(ctx, packet.UnsignedTx,
		[]*lndclient.SignDescriptor{{
			KeyDesc:       keychain.KeyDescriptor{KeyLocator: k.locator},
			SignMethod:    input.TaprootScriptSpendSignMethod,
			WitnessScript: spend.LeafScript,
			Output:        prevouts[index], InputIndex: int(index),
		}}, prevouts)
	if err != nil {
		return nil, err
	}
	if len(sigs) != 1 || len(sigs[0]) != schnorr.SignatureSize {
		return nil, errors.New("invalid reservation signature")
	}
	sig, err := schnorr.ParseSignature(sigs[0])
	if err != nil {
		return nil, err
	}
	hashes := txscript.NewTxSigHashes(packet.UnsignedTx, fetcher)
	hash, err := txscript.CalcTapscriptSignaturehash(
		hashes, txscript.SigHashDefault, packet.UnsignedTx, int(index),
		fetcher, txscript.NewBaseTapLeaf(spend.LeafScript),
	)
	if err != nil || !sig.Verify(hash, k.serverKey) {
		return nil, errors.New("reservation signature does not match")
	}
	witness, err := k.contract.TimeoutWitness(assetRoot, sigs[0])
	if err != nil {
		return nil, fmt.Errorf("reservation witness: %w", err)
	}
	return &SpendWitness{InputIndex: index, Witness: witness}, nil
}
