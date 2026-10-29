package tapsdk

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/loop/internal/assetproof"
	"github.com/lightninglabs/taproot-assets/proof"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

// PrepareInputs restores proof lineage and declares OP_TRUE inputs in tapd.
// The SDK builder owns full input proof verification; this does not repeat it.
func PrepareInputs(ctx context.Context, wallet *sdk.Wallet,
	request *sdk.CustomAnchorRequest) error {

	if request == nil {
		return swapkit.ErrInvalid
	}
	// Bound the complete request before decoding or importing any history.
	var size int
	var raws [][]byte
	for _, input := range request.Inputs {
		raw := input.ProofFile
		if input.ProofPath != nil {
			if len(raw) != 0 {
				return swapkit.ErrInvalid
			}
			if err := input.ProofPath.Validate(); err != nil {
				return err
			}
			encoded, err := input.ProofPath.MarshalBinary()
			if err != nil {
				return err
			}
			if len(encoded) > assetpolicy.MaxProvenanceBytes-size {
				return swapkit.ErrInvalid
			}
			size += len(encoded)
			raw = input.ProofPath.ConfirmedBaseProof
		} else {
			if len(raw) > assetpolicy.MaxProvenanceBytes-size {
				return swapkit.ErrInvalid
			}
			size += len(raw)
		}
		raws = append(raws, raw)
	}
	for _, passive := range request.PassiveAssets.Packets {
		if len(
			passive.ProofFile,
		) > assetpolicy.MaxProvenanceBytes-size {

			return swapkit.ErrInvalid
		}
		size += len(passive.ProofFile)
		raws = append(raws, passive.ProofFile)
	}
	request = request.Clone()
	for i := range raws {
		raws[i] = bytes.Clone(raws[i])
	}
	params, err := nodeParams(ctx, wallet)
	if err != nil {
		return err
	}
	var inventory []*proof.Proof
	for _, raw := range raws {
		if err := checkProvenanceWithParams(ctx, raw, params); err != nil {
			return err
		}
		p, err := LastProof(raw)
		if err != nil {
			return err
		}
		inventory = append(inventory, p)
	}
	if err := assetproof.VerifyCompleteAnchors(inventory); err != nil {
		return err
	}
	for _, input := range request.Inputs {
		raw := input.ProofFile
		var p *proof.Proof
		if input.ProofPath != nil {
			raw = input.ProofPath.ConfirmedBaseProof
			if len(input.ProofPath.Steps) > 0 {
				p, err = proof.Decode(
					input.ProofPath.Steps[len(input.ProofPath.Steps)-1].TransitionProof,
				)
			}
		}
		if p == nil && err == nil {
			p, err = LastProof(raw)
		}
		if err != nil {
			return err
		}
		if input.Witness.Mode == sdk.CustomAssetWitnessCallerProvided {
			pub, err := sdk.ParsePubKey(
				p.Asset.ScriptKey.PubKey.SerializeCompressed(),
			)
			if err != nil {
				return err
			}
			if err := declareOPTrue(ctx, wallet, pub, input.Witness.Stack); err != nil {
				return err
			}
		}
		if err := importProof(ctx, wallet, raw); err != nil {
			return err
		}
	}
	for _, passive := range request.PassiveAssets.Packets {
		if err := importProof(ctx, wallet, passive.ProofFile); err != nil {
			return err
		}
	}

	return nil
}

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

// commit rebuilds an already observed transfer's proof. It never signs a
// wallet transaction; the caller applies the observed Bitcoin witness.
func commit(
	ctx context.Context,
	wallet *sdk.Wallet,
	chain Chain,
	minConfs uint32,
	request *sdk.CustomAnchorRequest,
) (*sdk.CustomAnchorTransferPackage, error) {

	if err := PrepareInputs(ctx, wallet, request); err != nil {
		return nil, err
	}
	plan, err := wallet.NewCustomAnchorTxBuilder().Build(ctx, request)
	if err != nil {
		return nil, err
	}
	err = CheckInputsCurrent(ctx, chain, minConfs, request, false)
	if err != nil {
		return nil, err
	}
	return plan.Commit(ctx, sdk.CustomAnchorCommitOptions{
		Publish: sdk.CustomAnchorPublishMetadata{
			Label: "Loop asset transfer",
		},
	})
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
