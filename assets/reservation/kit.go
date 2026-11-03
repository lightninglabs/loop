package reservation

import (
	"bytes"
	"context"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/loop/swapkit/tapsdk"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/taprpc"
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
	contract *swapkit.DepositKit
	assetID  asset.ID
	params   *address.ChainParams
}

// NewKit constructs a server-funded CSV reservation. Explicit key roles put
// the server on the timeout path.
func NewKit(server, client *btcec.PublicKey,
	assetID asset.ID, csvDelay uint32,
	params *address.ChainParams) (*Kit, error) {

	if server == nil || client == nil || assetID == asset.ZeroID ||
		params == nil || params.Params == nil || params.TapHRP == "" {

		return nil, errors.New("invalid reservation contract")
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
		contract: contract,
		assetID:  assetID, params: assetpolicy.CloneAddressParams(*params),
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

// AnchorPlan identifies the Bitcoin contract for a reservation allocation.
func (k *Kit) AnchorPlan() swapkit.AnchorPlan {
	return k.contract.AnchorPlan()
}

// TimeoutSpend binds the server's refund to the exact isolated asset output.
func (k *Kit) TimeoutSpend(p *proof.Proof, amount uint64) (
	*swapkit.SwapAnchorSpendInfo, error) {

	if k == nil || k.contract == nil || p == nil {
		return nil, ErrInvalidReservation
	}
	key, descriptor, err := tapsdk.LegacyOPTrueDescriptor()
	if err != nil {
		return nil, err
	}
	anchor := k.AnchorPlan()
	point := p.OutPoint()
	binding := tapsdk.InputBinding{
		ID: "reservation", IssuanceID: sdk.AssetID(k.assetID),
		Amount: amount, ScriptKey: key.PubKey, Anchor: &anchor,
		AnchorOutpoint: sdk.Outpoint{
			Txid:  sdk.Hash(point.Hash),
			Index: point.Index,
		},
		OPTrue: descriptor,
	}
	if group := p.Asset.GroupKey; group != nil {
		key := sdk.PubKey(group.GroupPubKey.SerializeCompressed())
		binding.GroupKey = &key
	}
	root, err := binding.IsolatedAssetRoot(p)
	if err != nil {
		return nil, err
	}
	return k.contract.TimeoutSpend(root)
}
