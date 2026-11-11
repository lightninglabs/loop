package tapsdk

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/internal/assetproof"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/proof"
)

// CheckInputsCurrent repeats chain checks after SDK Build's verification RPCs.
// Call it before Commit so verification cannot authorize a now stale input.
func CheckInputsCurrent(ctx context.Context, chain Chain, minConfs uint32,
	request *sdk.CustomAnchorRequest, unspent bool) error {

	if request == nil {
		return swapkit.ErrInvalid
	}
	raws := make(
		[][]byte,
		0,
		len(request.Inputs)+len(request.PassiveAssets.Packets),
	)
	for _, input := range request.Inputs {
		raw := input.ProofFile
		if input.ProofPath != nil {
			if len(input.ProofPath.Steps) > 0 {
				return fmt.Errorf(
					"compact inputs require ApprovedParents.CheckCurrent",
				)
			}
			raw = input.ProofPath.ConfirmedBaseProof
		}
		raws = append(raws, raw)
	}
	for _, passive := range request.PassiveAssets.Packets {
		raws = append(raws, passive.ProofFile)
	}
	var inventory []*proof.Proof
	for _, raw := range raws {
		p, err := LastProof(raw)
		if err != nil {
			return err
		}
		inventory = append(inventory, p)
		if err := checkConfirmation(ctx, chain, minConfs, p, unspent); err != nil {
			return err
		}
	}
	if err := assetproof.VerifyCompleteAnchors(inventory); err != nil {
		return err
	}
	if unspent {
		packet, err := DecodePSBT(request.AnchorPSBT)
		if err != nil {
			return err
		}
		return checkFundingCurrent(ctx, chain, packet, nil)
	}

	return nil
}

// checkFundingCurrent checks all asset and Bitcoin-only funding inputs. Skip
// contains only the outputs supplied by independently approved signed parents.
func checkFundingCurrent(ctx context.Context, chain Chain, packet *psbt.Packet,
	skip map[wire.OutPoint]bool) error {

	if chain == nil || packet == nil || packet.UnsignedTx == nil ||
		len(packet.Inputs) != len(packet.UnsignedTx.TxIn) {

		return swapkit.ErrInvalid
	}
	seen := make(map[wire.OutPoint]bool)
	for _, input := range packet.UnsignedTx.TxIn {
		if input == nil {
			return swapkit.ErrInvalid
		}
		point := input.PreviousOutPoint
		if seen[point] {
			return fmt.Errorf("duplicate Bitcoin funding input")
		}
		seen[point] = true
		if skip[point] {
			continue
		}
		available, err := chain.Unspent(ctx, point)
		if err != nil {
			return err
		}
		if !available {
			return swapkit.ErrPending
		}
	}
	return nil
}
