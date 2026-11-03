package tapsdk

import (
	"bytes"
	"context"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

// FundingProof selects the exact confirmed wallet output for a deposit.
// It inspects Loop policy; the subsequent SDK Build verifies lineage once.
func FundingProof(ctx context.Context, wallet *sdk.Wallet,
	chain Chain, minConfs uint32,
	request swapkit.CreateDepositRequest) ([]byte, error) {

	if err := CheckNodeVersion(ctx, wallet); err != nil {
		return nil, err
	}
	ref := sdk.AssetRefFromGroupKey(request.GroupKey)
	assets, err := wallet.Client().ListAssetRecords(ctx,
		&sdk.ListAssetsRequest{AssetRef: &ref})
	if err != nil {
		return nil, err
	}
	for _, row := range assets {
		if row == nil || row.Amount != request.Amount ||
			row.AnchorOutpoint == nil {

			continue
		}
		file, err := wallet.ExportProofFile(ctx,
			sdk.AssetRefFromAssetID(row.Genesis.IssuanceID),
			row.ScriptKey.PubKey, row.AnchorOutpoint)
		if err != nil || file == nil {
			continue
		}
		if err := checkProvenance(ctx, wallet, file.RawProofFile); err != nil {
			continue
		}
		p, err := inspectProof(ctx, chain, minConfs, file.RawProofFile,
			request.GroupKey, request.Amount, true)
		if err != nil {
			continue
		}
		scriptKey := p.Asset.ScriptKey.PubKey.SerializeCompressed()
		if p.OutPoint() != SDKPoint(*row.AnchorOutpoint) ||
			sdk.AssetID(p.Asset.ID()) != row.Genesis.IssuanceID ||
			!bytes.Equal(scriptKey, row.ScriptKey.PubKey[:]) {

			continue
		}
		return file.RawProofFile, nil
	}
	return nil, swapkit.ErrInvalid
}
