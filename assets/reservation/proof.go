package reservation

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/deposit"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
)

// VerifyReservationProof binds the terminal asset to the shared deposit script,
// amount, and outpoint, then verifies its full history with tapd. RPC failures
// remain retryable; invalid proofs are reported as ErrInvalidReservation.
func VerifyReservationProof(ctx context.Context, verifier deposit.ProofVerifier,
	kit *deposit.Kit, raw []byte, outpoint wire.OutPoint,
	amount uint64) (*proof.Proof, error) {

	if verifier == nil || kit == nil || len(raw) == 0 || len(raw) > 16<<20 {
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
	checked, err := verifier.VerifyProof(ctx, &taprpc.ProofFile{
		RawProofFile: bytes.Clone(raw),
	})
	if err != nil {
		return nil, err
	}
	if checked == nil || !checked.Valid {
		return nil, ErrInvalidReservation
	}
	return terminal, nil
}
