package tapsdk

import (
	"context"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/keychain"
)

// RecoveryRequest constructs an SDK refund or claim to the normal wallet key.
func RecoveryRequest(ctx context.Context, wallet *sdk.Wallet,
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
	_, err = wallet.Client().DeclareScriptKey(ctx, &sdk.DeclareScriptKeyRequest{
		ScriptKey: recipient.Script.External.ScriptKey,
	})
	if err != nil {
		return nil, err
	}
	p, err := LastProof(inputProof)
	if err != nil {
		return nil, err
	}
	anchor, err := AnchorTemplate(
		p, recipient.Anchor.InternalKey, value, spend,
	)
	if err != nil {
		return nil, err
	}
	return CustomRequest(assetInput(out, inputProof), recipient, anchor,
		ScriptSigningPlan(spend, 0)), nil
}
