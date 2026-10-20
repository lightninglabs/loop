package runtime

import (
	"context"

	"github.com/lightninglabs/loop/swapkit/bridge"
	kit "github.com/lightninglabs/loop/swapkit/tapsdk"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/keychain"
)

// recoveryPlan returns a wallet-owned output for either a deposit refund or
// an HTLC spend. The caller persists the plan before committing it.
func (r *Runtime) recoveryPlan(ctx context.Context,
	out sdk.CustomAnchorAssetOutputSummary, inputProof []byte, public [33]byte,
	key keychain.KeyLocator, spend *kit.SwapAnchorSpendInfo) (
	*sdk.CustomAnchorRequest, error) {

	value := out.AnchorValueSat - int64(r.cfg.FeeSat)
	if value < 330 {
		return nil, bridge.ErrInvalid
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
		spend.SigningPlan(0)), nil
}

// signScriptSpend shares the durable commit/sign sequence for every unilateral
// path. Persist the exact committed package before signing and the completed
// transaction before returning it to a caller that can publish it.
func (r *Runtime) signScriptSpend(ctx context.Context,
	plan *sdk.CustomAnchorRequest, tx *transaction, key keychain.KeyLocator,
	preimage *[32]byte, save func() error) error {

	if tx.Package == nil {
		pkg, err := r.commit(ctx, plan)
		if err != nil {
			return err
		}
		tx.Package = pkg
		if err := save(); err != nil {
			return err
		}
	}
	if len(tx.Final) != 0 {
		return nil
	}
	requests, err := tx.Package.SigningRequests()
	if err != nil || len(requests.ScriptPath) != 1 {
		return bridge.ErrInvalid
	}
	request := requests.ScriptPath[0]
	signature, err := r.cfg.Signer.ScriptPath(
		ctx, key, tx.Package, request.ID,
	)
	if err != nil {
		return err
	}
	// The SDK adds the sealed script and control block, then executes the
	// complete witness. Refunds need only the signature; claims also need
	// the preimage, whose hash is enforced by that committed script.
	unlock := [][]byte{signature}
	if preimage != nil {
		unlock = append([][]byte{preimage[:]}, unlock...)
	}
	final, err := tx.Package.ApplyScriptPathWitness(request.ID, unlock)
	if err != nil {
		return err
	}
	tx.Final = final.AnchorPsbt
	return save()
}
