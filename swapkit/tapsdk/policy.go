package tapsdk

import (
	"bytes"
	"context"
	"fmt"

	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/proof"
)

// CheckNodeVersion checks the live tapd compatibility floor for new on-chain
// asset operations. Local decoding and recovery of saved Bitcoin witnesses do
// not require a running node or change already funded contracts.
func CheckNodeVersion(ctx context.Context, wallet *sdk.Wallet) error {
	_, err := nodeParams(ctx, wallet)
	return err
}

func nodeParams(ctx context.Context, wallet *sdk.Wallet) (
	*address.ChainParams, error) {

	if wallet == nil || wallet.Client() == nil {
		return nil, fmt.Errorf("tapd wallet is required")
	}
	info, err := wallet.Client().GetInfo(ctx)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, fmt.Errorf("tapd omitted node information")
	}
	if err := assetpolicy.CheckOnchainSwapTapdVersion(info.Version); err != nil {
		return nil, err
	}
	var params address.ChainParams
	switch info.Network {
	case "mainnet":
		params = address.MainNetTap
	case "testnet", "testnet3":
		params = address.TestNet3Tap
	case "testnet4":
		params = address.TestNet4Tap
	case "regtest":
		params = address.RegressionNetTap
	case "signet":
		params = address.SigNetTap
	case "simnet":
		params = address.SimNetTap
	default:
		return nil, fmt.Errorf(
			"unsupported tapd network %q",
			info.Network,
		)
	}
	return assetpolicy.CanonicalAddressParams(&params)
}

func checkProvenance(
	ctx context.Context,
	wallet *sdk.Wallet,
	raw []byte,
) error {

	if err := assetpolicy.CheckProofFileSize(raw); err != nil {
		return err
	}
	params, err := nodeParams(ctx, wallet)
	if err != nil {
		return err
	}
	return checkProvenanceWithParams(ctx, raw, params)
}

func checkProvenanceWithParams(
	ctx context.Context,
	raw []byte,
	params *address.ChainParams,
) error {

	if err := assetpolicy.CheckProofFileSize(raw); err != nil {
		return err
	}
	file, err := proof.DecodeFile(raw)
	if err != nil {
		return err
	}
	return assetpolicy.ValidateProvenance(ctx, file, params)
}

func checkVerifiedSummary(
	result *sdk.VerifyProofResponse,
	p *proof.Proof,
) error {

	if result == nil || !result.Valid || result.DecodedProof == nil ||
		p == nil {

		return fmt.Errorf(
			"%w: tapd did not verify the selected proof",
			swapkit.ErrInvalid,
		)
	}
	decoded := result.DecodedProof
	var group *sdk.PubKey
	if p.Asset.GroupKey != nil {
		key, err := sdk.ParsePubKey(
			p.Asset.GroupKey.GroupPubKey.SerializeCompressed(),
		)
		if err != nil {
			return err
		}
		group = &key
	}
	ref := sdk.AssetRefFromTypedAsset(
		sdk.AssetID(p.Asset.ID()),
		group,
		sdk.AssetType(p.Asset.Type),
	)
	if decoded.IssuanceID != sdk.AssetID(p.Asset.ID()) ||
		!decoded.AssetRef.Equivalent(ref) ||
		decoded.Amount != p.Asset.Amount ||
		SDKPoint(decoded.Outpoint) != p.OutPoint() ||
		!bytes.Equal(decoded.ScriptKey[1:], p.Asset.ScriptKey.PubKey.SerializeCompressed()[1:]) {

		return fmt.Errorf(
			"%w: tapd verified a different asset output",
			swapkit.ErrInvalid,
		)
	}
	return nil
}
