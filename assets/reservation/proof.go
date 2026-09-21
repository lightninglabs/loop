package reservation

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/deposit"
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
func VerifyReservationProof(ctx context.Context, verifier deposit.ProofVerifier,
	kit *deposit.Kit, raw []byte, outpoint wire.OutPoint,
	amount uint64) (*proof.Proof, error) {

	if verifier == nil || kit == nil || len(raw) == 0 || len(raw) > maxReservationProofSize {
		return nil, ErrInvalidReservation
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
	if _, err := kit.AnchorRootFromProofCommitment(terminal, amount); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReservation, err)
	}
	// The shared kit rejects ownership-only and incomplete histories,
	// including nested inputs, before asking tapd to verify them. Retain the
	// RPC error separately so transport failures remain retryable.
	attempt := &proofVerificationAttempt{ProofVerifier: verifier}
	_, err = kit.VerifyProofFile(ctx, attempt, &taprpc.ProofFile{
		RawProofFile: raw,
	}, &outpoint, amount)
	if attempt.rpcErr != nil {
		return nil, attempt.rpcErr
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidReservation, err)
	}
	return terminal, nil
}

// proofVerificationAttempt distinguishes node errors from invalid proof data.
type proofVerificationAttempt struct {
	deposit.ProofVerifier

	rpcErr error
}

func (v *proofVerificationAttempt) VerifyProof(ctx context.Context,
	req *taprpc.ProofFile, opts ...grpc.CallOption) (
	*taprpc.VerifyProofResponse, error) {

	response, err := v.ProofVerifier.VerifyProof(ctx, req, opts...)
	v.rpcErr = err
	return response, err
}
