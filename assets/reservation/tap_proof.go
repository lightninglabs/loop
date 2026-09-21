package reservation

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/loop/assets/deposit"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
	"google.golang.org/grpc"
)

// ProofClient verifies proofs and records verified issuance metadata in the
// client's local tapd. Recording issuance does not import spendable assets.
type ProofClient interface {
	deposit.ProofVerifier
	InsertProof(context.Context, *universerpc.AssetProof,
		...grpc.CallOption) (*universerpc.AssetProofResponse, error)
}

type tapProofVerifier struct{ tap ProofClient }

// NewTapProofVerifier wraps the local tapd client to register issuance metadata
// before verifying a reservation's full proof history.
func NewTapProofVerifier(tap ProofClient) deposit.ProofVerifier {
	return tapProofVerifier{tap: tap}
}

// VerifyProof supplies issuance metadata before full verification. Tapd
// v0.8.3 needs that metadata to return a decoded transfer proof, including on
// a BTC-only client that has never received this asset. InsertProof verifies
// the issuance against tapd's chain view and safely repeats after a restart.
func (v tapProofVerifier) VerifyProof(ctx context.Context, req *taprpc.ProofFile,
	opts ...grpc.CallOption) (*taprpc.VerifyProofResponse, error) {

	if req == nil || len(req.RawProofFile) == 0 ||
		len(req.RawProofFile) > maxReservationProofSize {

		return nil, ErrInvalidReservation
	}
	file, err := proof.DecodeFile(req.RawProofFile)
	if err != nil {
		return nil, ErrInvalidReservation
	}
	first, err := file.ProofAt(0)
	if err != nil || !first.Asset.IsGenesisAsset() ||
		first.Asset.ScriptKey.PubKey == nil {

		return nil, ErrInvalidReservation
	}
	last, err := file.LastProof()
	if err != nil || first.Asset.ID() != last.Asset.ID() {
		return nil, ErrInvalidReservation
	}
	raw, err := file.RawProofAt(0)
	if err != nil {
		return nil, ErrInvalidReservation
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
	_, err = v.tap.InsertProof(ctx, &universerpc.AssetProof{
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
	return v.tap.VerifyProof(ctx, req, opts...)
}
