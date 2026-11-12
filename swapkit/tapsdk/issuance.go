package tapsdk

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
	"google.golang.org/grpc"
)

// RPCProofVerifier verifies full proof files with tapd. Verification alone
// does not establish the caller's expected asset, amount, contract or chain state.
type RPCProofVerifier interface {
	VerifyProof(context.Context, *taprpc.ProofFile,
		...grpc.CallOption) (*taprpc.VerifyProofResponse, error)
}

// IssuanceProofClient records verified issuance metadata and verifies history.
// Inserting issuance metadata does not register a received wallet asset.
type IssuanceProofClient interface {
	RPCProofVerifier
	InsertProof(context.Context, *universerpc.AssetProof,
		...grpc.CallOption) (*universerpc.AssetProofResponse, error)
}

// VerifyProofWithIssuance registers the first issuance before asking tapd to
// verify the full history. This supports assets unknown to the local node and
// safely repeats after restart. Callers enforce their transport limits and
// expected output before calling; node errors are returned unchanged.
func VerifyProofWithIssuance(ctx context.Context, tap IssuanceProofClient,
	req *taprpc.ProofFile, opts ...grpc.CallOption) (
	*taprpc.VerifyProofResponse, error) {

	if tap == nil || req == nil || len(req.RawProofFile) == 0 {
		return nil, swapkit.ErrInvalid
	}
	if err := assetpolicy.CheckProofFileSize(req.RawProofFile); err != nil {
		return nil, swapkit.ErrInvalid
	}
	file, err := proof.DecodeFile(req.RawProofFile)
	if err != nil {
		return nil, swapkit.ErrInvalid
	}
	first, err := file.ProofAt(0)
	if err != nil || !first.Asset.IsGenesisAsset() ||
		first.Asset.ScriptKey.PubKey == nil {

		return nil, swapkit.ErrInvalid
	}
	last, err := file.LastProof()
	if err != nil || first.Asset.ID() != last.Asset.ID() {
		return nil, swapkit.ErrInvalid
	}
	raw, err := file.RawProofAt(0)
	if err != nil {
		return nil, swapkit.ErrInvalid
	}
	assetID := first.Asset.ID()
	id := &universerpc.ID{
		Id: &universerpc.ID_AssetId{
			AssetId: assetID[:],
		},
		ProofType: universerpc.ProofType_PROOF_TYPE_ISSUANCE,
	}
	if first.Asset.GroupKey != nil {
		id.Id = &universerpc.ID_GroupKey{
			GroupKey: schnorr.SerializePubKey(
				&first.Asset.GroupKey.GroupPubKey,
			),
		}
	}
	_, err = tap.InsertProof(ctx, &universerpc.AssetProof{
		Key: &universerpc.UniverseKey{
			Id: id,
			LeafKey: &universerpc.AssetKey{
				Outpoint: &universerpc.AssetKey_OpStr{
					OpStr: first.OutPoint().String(),
				},
				ScriptKey: &universerpc.AssetKey_ScriptKeyBytes{
					ScriptKeyBytes: schnorr.SerializePubKey(
						first.Asset.ScriptKey.PubKey,
					),
				},
			},
		},
		AssetLeaf: &universerpc.AssetLeaf{
			Proof: raw,
		},
	}, opts...)
	if err != nil {
		return nil, err
	}
	return tap.VerifyProof(ctx, req, opts...)
}
