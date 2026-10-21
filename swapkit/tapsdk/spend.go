package tapsdk

import (
	"context"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/keychain"
)

// recoveryPlan returns a wallet-owned output for either a deposit refund or
// an HTLC spend.
func (r *Driver) recoveryPlan(ctx context.Context,
	out sdk.CustomAnchorAssetOutputSummary, inputProof []byte, public [33]byte,
	key keychain.KeyLocator, spend *swapkit.SwapAnchorSpendInfo, fee uint64) (
	*sdk.CustomAnchorRequest, error) {

	if out.AnchorValueSat < 330 ||
		fee > uint64(out.AnchorValueSat-330) {

		return nil, swapkit.ErrInvalid
	}
	value := out.AnchorValueSat - int64(fee)
	if fee == 0 {
		return nil, swapkit.ErrInvalid
	}
	recipient, err := walletOutput(public, int32(key.Family), key.Index,
		out.AssetRef, out.Amount, value)
	if err != nil {
		return nil, err
	}
	// Declare the locator so normal wallet operations can spend the
	// recovered asset after proof import.
	_, err = r.cfg.SDK.DeclareScriptKey(ctx, &sdk.DeclareScriptKeyRequest{
		ScriptKey: recipient.Script.External.ScriptKey,
	})
	if err != nil {
		return nil, err
	}
	p, err := lastProof(inputProof)
	if err != nil {
		return nil, err
	}
	anchor, err := anchorTemplate(
		p, recipient.Anchor.InternalKey, value, spend,
	)
	if err != nil {
		return nil, err
	}
	return customRequest(assetInput(out, inputProof), recipient, anchor,
		ScriptSigningPlan(spend, 0)), nil
}

// signScriptSpend signs one committed unilateral spend. Its caller owns
// persistence of the completed transaction before publication.
func (r *Driver) signScriptSpend(ctx context.Context,
	pkg *sdk.CustomAnchorTransferPackage, key keychain.KeyLocator,
	preimage *[32]byte) (*sdk.CustomAnchorTransferPackage, error) {

	final, err := SignScriptPath(ctx, r.cfg.Signer, key, pkg, preimage)
	if err != nil {
		return nil, err
	}
	packet, err := decodePSBT(final)
	if err != nil {
		return nil, err
	}
	if err := psbt.MaybeFinalizeAll(packet); err != nil {
		return nil, err
	}
	final, err = encodePSBT(packet)
	if err != nil {
		return nil, err
	}
	return pkg.WithFinalAnchorPSBT(final)
}
