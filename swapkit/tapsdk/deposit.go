package tapsdk

import (
	"bytes"
	"context"
	"errors"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

// FundingProof selects the exact confirmed wallet output for a deposit.
// It inspects Loop policy; the subsequent SDK Build verifies lineage once.
// ErrInvalid means the wallet holds no matching unspent output. A matching
// output that only waits on the chain returns ErrPending for outstanding
// confirmations, or the chain backend's own error, so the caller can retry.
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
	// waiting is the first chain-state failure of an exact candidate. It is
	// reported only when no candidate is ready.
	var waiting error
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
		p, err := inspectAsset(file.RawProofFile, request.GroupKey,
			request.Amount)
		if err != nil {
			continue
		}
		scriptKey := p.Asset.ScriptKey.PubKey.SerializeCompressed()
		if p.OutPoint() != SDKPoint(*row.AnchorOutpoint) ||
			sdk.AssetID(p.Asset.ID()) != row.Genesis.IssuanceID ||
			!bytes.Equal(scriptKey, row.ScriptKey.PubKey[:]) {

			continue
		}
		// This is the exact wallet output. An outstanding confirmation or
		// a chain backend failure does not make the request invalid,
		// while a spent output can never fund the deposit.
		err = checkConfirmation(ctx, chain, minConfs, p, false)
		if err == nil {
			var unspent bool
			unspent, err = chain.Unspent(ctx, p.OutPoint())
			if err == nil && !unspent {
				continue
			}
		}
		if err != nil {
			if waiting == nil && !errors.Is(err, swapkit.ErrInvalid) {
				waiting = err
			}
			continue
		}
		return file.RawProofFile, nil
	}
	if waiting != nil {
		return nil, waiting
	}
	return nil, swapkit.ErrInvalid
}
