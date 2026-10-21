package tapsdk

import (
	"context"

	sdk "github.com/lightninglabs/tap-sdk"
)

// PrepareInputs restores proof lineage and declares OP_TRUE inputs in tapd.
// The SDK builder owns full input proof verification; this does not repeat it.
func PrepareInputs(ctx context.Context, wallet *sdk.Wallet,
	request *sdk.CustomAnchorRequest) error {

	for _, input := range request.Inputs {
		if input.Witness.Mode == sdk.CustomAssetWitnessCallerProvided {
			p, err := LastProof(input.ProofFile)
			if err != nil {
				return err
			}
			pub, err := sdk.ParsePubKey(p.Asset.ScriptKey.PubKey.SerializeCompressed())
			if err != nil {
				return err
			}
			if err := declareOPTrue(ctx, wallet, pub, input.Witness.Stack); err != nil {
				return err
			}
		}
		if err := importProof(ctx, wallet, input.ProofFile); err != nil {
			return err
		}
	}
	return nil
}

// CheckInputsCurrent repeats chain checks after SDK Build's verification RPCs.
// Call it before Commit so verification cannot authorize a now stale input.
func CheckInputsCurrent(ctx context.Context, chain Chain, minConfs uint32,
	request *sdk.CustomAnchorRequest, unspent bool) error {

	for _, input := range request.Inputs {
		p, err := LastProof(input.ProofFile)
		if err != nil {
			return err
		}
		if err := checkConfirmation(ctx, chain, minConfs, p, unspent); err != nil {
			return err
		}
	}
	return nil
}

// commit rebuilds an already observed transfer's proof. It never signs a
// wallet transaction; the caller applies the observed Bitcoin witness.
func commit(ctx context.Context, wallet *sdk.Wallet,
	chain Chain, minConfs uint32,
	request *sdk.CustomAnchorRequest) (*sdk.CustomAnchorTransferPackage, error) {

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
		Publish: sdk.CustomAnchorPublishMetadata{Label: "Loop asset transfer"},
	})
}
