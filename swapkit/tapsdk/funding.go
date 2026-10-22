package tapsdk

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit"
)

// PrevoutSource supplies current, authenticated Bitcoin funding outputs. Its
// lookup includes mempool spends and returns nil for a spent/unknown output.
// Wallet-only ListUnspent cannot authenticate external contract anchors.
type PrevoutSource interface {
	Prevout(context.Context, wire.OutPoint) (*wire.TxOut, error)
}

// CheckFundingInputs binds every native PSBT WitnessUtxo to its current Bitcoin
// backend output. Use plan.AnchorPSBT after Build and repeat before Commit, or
// check an already populated request.AnchorPSBT. This authenticates values and
// scripts in addition to Chain.Unspent's existence check. Compact children use
// ApprovedParents for their not-yet-broadcast parent outputs instead.
func CheckFundingInputs(ctx context.Context, source PrevoutSource,
	anchorPSBT []byte) error {

	if source == nil {
		return fmt.Errorf("authenticated Bitcoin prevouts are required")
	}
	packet, err := DecodePSBT(anchorPSBT)
	if err != nil {
		return err
	}
	seen := make(map[wire.OutPoint]bool)
	for i, input := range packet.UnsignedTx.TxIn {
		if input == nil || i >= len(packet.Inputs) ||
			packet.Inputs[i].WitnessUtxo == nil {

			return swapkit.ErrInvalid
		}
		point := input.PreviousOutPoint
		if seen[point] {
			return fmt.Errorf("duplicate Bitcoin funding input")
		}
		seen[point] = true
		actual, err := source.Prevout(ctx, point)
		if err != nil {
			return err
		}
		if actual == nil {
			return swapkit.ErrPending
		}
		expected := packet.Inputs[i].WitnessUtxo
		if actual.Value < 0 || actual.Value != expected.Value ||
			!bytes.Equal(actual.PkScript, expected.PkScript) {

			return fmt.Errorf(
				"%w: Bitcoin prevout differs from authenticated backend",
				swapkit.ErrInvalid,
			)
		}
	}
	return nil
}
