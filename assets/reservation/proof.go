package reservation

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
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

	if verifier == nil || kit == nil || len(raw) == 0 ||
		len(raw) > maxReservationProofSize {

		return nil, ErrInvalidReservation
	}
	if err := assetpolicy.RequireOnchainSwapTapd(ctx, verifier); err != nil {
		return nil, err
	}
	raw = bytes.Clone(raw)
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
	if _, err := kit.TimeoutSpend(terminal, amount); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReservation, err)
	}
	// The shared kit rejects ownership-only and incomplete histories,
	// including nested inputs, before asking tapd to verify them. Retain the
	// RPC error separately so transport failures remain retryable.
	response, err := verifier.VerifyProof(ctx, &taprpc.ProofFile{
		RawProofFile: raw,
	})
	if err != nil {
		return nil, err
	}
	if response == nil || !response.Valid {
		return nil, ErrInvalidReservation
	}
	// Bind the authenticated node's answer to the exact locally checked
	// proof before the FSM can accept delivery.
	a := response.GetDecodedProof().GetAsset()
	id := terminal.Asset.ID()
	if a == nil || !bytes.Equal(a.GetAssetGenesis().GetAssetId(), id[:]) ||
		a.GetAmount() != amount || a.GetChainAnchor().GetAnchorOutpoint() != outpoint.String() ||
		!bytes.Equal(
			a.GetScriptKey(),
			terminal.Asset.ScriptKey.PubKey.SerializeCompressed(),
		) {

		return nil, ErrInvalidReservation
	}
	return terminal, nil
}
