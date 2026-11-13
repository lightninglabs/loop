package reservation

import (
	"context"
	"errors"

	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/loop/swapkit/tapsdk"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"google.golang.org/grpc"
)

// ProofClient verifies proofs and records verified issuance metadata in the
// client's local tapd. Recording issuance does not import spendable assets.
type ProofClient interface {
	ProofVerifier
	tapsdk.IssuanceProofClient
}

type tapProofVerifier struct{ tap ProofClient }

// NewTapProofVerifier wraps the local tapd client to register issuance metadata
// before verifying a reservation's full proof history.
func NewTapProofVerifier(tap ProofClient) ProofVerifier {
	return tapProofVerifier{tap: tap}
}

// GetInfo passes through the version check before a new purchase begins.
func (v tapProofVerifier) GetInfo(ctx context.Context,
	req *taprpc.GetInfoRequest, opts ...grpc.CallOption) (
	*taprpc.GetInfoResponse, error) {

	return v.tap.GetInfo(ctx, req, opts...)
}

// VerifyProof supplies issuance metadata before full verification. Tapd
// needs that metadata to return a decoded transfer proof, including on
// a BTC-only client that has never received this asset. InsertProof verifies
// the issuance against tapd's chain view and safely repeats after a restart.
func (v tapProofVerifier) VerifyProof(ctx context.Context, req *taprpc.ProofFile,
	opts ...grpc.CallOption) (*taprpc.VerifyProofResponse, error) {

	if req == nil || len(req.RawProofFile) == 0 ||
		len(req.RawProofFile) > maxReservationProofSize {

		return nil, ErrInvalidReservation
	}
	result, err := tapsdk.VerifyProofWithIssuance(ctx, v.tap, req, opts...)
	if errors.Is(err, swapkit.ErrInvalid) {
		return nil, ErrInvalidReservation
	}
	return result, err
}
