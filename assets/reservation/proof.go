package reservation

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"google.golang.org/grpc"
)

const (
	// maxReservationProofSize bounds the raw proof accepted by the verifier.
	maxReservationProofSize = 16 << 20

	// Allow room for the reservation ID, outpoint, and protobuf framing in
	// addition to a maximum-sized proof. This applies only to the proof RPC.
	maxReservationProofMessageSize = maxReservationProofSize + 1024
)

// VerifyReservationProof binds the terminal asset to the shared deposit script,
// amount, and outpoint, then verifies its full history with tapd. RPC failures
// remain retryable; invalid proofs are reported as ErrInvalidReservation.
func VerifyReservationProof(ctx context.Context, verifier ProofVerifier,
	kit *Kit, raw []byte, outpoint wire.OutPoint,
	amount uint64) (*proof.Proof, error) {

	if verifier == nil || kit == nil || len(raw) == 0 || len(raw) > maxReservationProofSize {
		return nil, ErrInvalidReservation
	}
	if err := assetpolicy.RequireOnchainSwapTapd(ctx, verifier); err != nil {
		return nil, err
	}
	file, err := proof.DecodeFile(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReservation, err)
	}
	terminal, err := file.LastProof()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReservation, err)
	}
	if terminal.OutPoint() != outpoint || terminal.BlockHeight == 0 {
		return nil, ErrInvalidReservation
	}
	if err := assetpolicy.ValidateProvenance(ctx, file, kit.params); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReservation, err)
	}
	if _, err := kit.AnchorRootFromProofCommitment(terminal, amount); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReservation, err)
	}
	// The shared kit rejects ownership-only and incomplete histories,
	// including nested inputs, before asking tapd to verify them. Retain the
	// RPC error separately so transport failures remain retryable.
	attempt := &proofVerificationAttempt{ProofVerifier: verifier}
	response, err := attempt.VerifyProof(ctx, &taprpc.ProofFile{
		RawProofFile: raw,
	})
	if attempt.rpcErr != nil {
		return nil, attempt.rpcErr
	}
	if err != nil || response == nil || !response.Valid {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidReservation, err)
	}
	return terminal, nil
}

// proofVerificationAttempt distinguishes node errors from invalid proof data.
type proofVerificationAttempt struct {
	ProofVerifier

	rpcErr error
}

func (v *proofVerificationAttempt) VerifyProof(ctx context.Context,
	req *taprpc.ProofFile, opts ...grpc.CallOption) (
	*taprpc.VerifyProofResponse, error) {

	response, err := v.ProofVerifier.VerifyProof(ctx, req, opts...)
	v.rpcErr = err
	return response, err
}
