package loopin

import (
	"context"

	"github.com/lightninglabs/loop/swapkit"
	assetkit "github.com/lightninglabs/loop/swapkit/tapsdk"
	sdk "github.com/lightninglabs/tap-sdk"
)

// assetSwap checks the deposit owned by this FSM against the negotiated
// contract. Builders validate lineage themselves; other operations verify it
// here. The result is never retained across actions or process restarts.
func (f *FSM) assetSwap(ctx context.Context, offer swapkit.Offer,
	raw []byte, build, unspent bool) (*assetkit.SwapContext, error) {

	if len(f.loopIn.Deposits) != 1 {
		return nil, swapkit.ErrInvalid
	}
	asset := f.loopIn.Deposits[0].AssetSnapshot()
	if asset == nil {
		return nil, swapkit.ErrInvalid
	}
	d, err := assetkit.LoadDeposit(*asset, f.loopIn.HtlcKeyLocator)
	if err != nil {
		return nil, err
	}
	cfg := f.cfg.Assets
	if build {
		err = assetkit.InspectDeposit(
			ctx, cfg.Chain, cfg.MinConfs, d, unspent,
		)
	} else {
		err = assetkit.VerifyDeposit(ctx, cfg.Wallet, cfg.Chain,
			cfg.MinConfs, d, unspent)
	}
	if err != nil {
		return nil, err
	}
	return assetkit.NewSwap(offer, d, raw)
}

func (f *FSM) validateAssetOffer(ctx context.Context,
	offer swapkit.Offer) error {

	_, err := f.assetSwap(ctx, offer, nil, false, true)
	return err
}

func (f *FSM) prepareAssetRecovery(ctx context.Context) ([]byte, error) {
	cfg, a := f.cfg.Assets, f.loopIn.Asset
	s, err := f.assetSwap(ctx, a.Offer, nil, true, true)
	if err != nil {
		return nil, err
	}
	request, err := assetkit.FallbackRequest(s, cfg.FeeSat, cfg.MaxFeeSat)
	if err != nil {
		return nil, err
	}
	if err := assetkit.PrepareInputs(ctx, cfg.Wallet, request); err != nil {
		return nil, err
	}
	plan, err := cfg.Wallet.NewCustomAnchorTxBuilder().Build(ctx, request)
	if err != nil {
		return nil, err
	}

	if err := assetkit.CheckInputsCurrent(ctx, cfg.Chain, cfg.MinConfs,
		request, true); err != nil {
		return nil, err
	}
	pkg, err := plan.Commit(ctx, sdk.CustomAnchorCommitOptions{})
	if err != nil {
		return nil, err
	}
	return pkg.MarshalBinary()
}

func (f *FSM) completedAssetSwap(ctx context.Context) (
	*assetkit.SwapContext, error) {

	a := f.loopIn.Asset
	s, err := f.assetSwap(ctx, a.Offer, a.RecoveryRequest, false, false)
	if err != nil {
		return nil, err
	}
	s.Fallback, err = assetkit.BindFinalPackage(s.Fallback, a.Fallback)
	return s, err
}

func (f *FSM) lookupAssetChainPreimage(ctx context.Context) (
	swapkit.PreimageResult, error) {

	s, err := f.completedAssetSwap(ctx)
	if err != nil {
		return swapkit.PreimageResult{}, err
	}
	return assetkit.DiscoverPreimage(ctx, f.cfg.Assets.Chain, s)
}

func (f *FSM) refundAssetSwap(ctx context.Context) ([]byte, error) {
	cfg, a := f.cfg.Assets, f.loopIn.Asset
	height, err := cfg.Chain.Height(ctx)
	if err != nil {
		return nil, err
	}
	if height < a.Offer.Terms.RefundHeight {
		return nil, swapkit.ErrPending
	}
	s, err := f.completedAssetSwap(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := assetkit.FallbackProof(ctx, cfg.Wallet, cfg.Chain,
		cfg.MinConfs, s, false)
	if err != nil {
		return nil, err
	}
	policy, err := assetkit.HTLC(a.Offer, s.Deposit.Contract)
	if err != nil {
		return nil, err
	}
	output := s.Fallback.Outputs[0]
	spend, err := policy.TimeoutSpend(output.TaprootAssetRoot)
	if err != nil {
		return nil, err
	}
	fee := s.Deposit.Contract.Output.AnchorValueSat - output.AnchorValueSat
	if fee <= 0 {
		return nil, swapkit.ErrInvalid
	}
	request, err := assetkit.RecoveryRequest(ctx, cfg.Wallet, output, raw,
		s.Deposit.Contract.ClientKey, s.Deposit.Key, spend, uint64(fee))
	if err != nil {
		return nil, err
	}
	if err := assetkit.PrepareInputs(ctx, cfg.Wallet, request); err != nil {
		return nil, err
	}
	plan, err := cfg.Wallet.NewCustomAnchorTxBuilder().Build(ctx, request)
	if err != nil {
		return nil, err
	}

	if err := assetkit.CheckInputsCurrent(ctx, cfg.Chain, cfg.MinConfs,
		request, false); err != nil {
		return nil, err
	}
	pkg, err := plan.Commit(ctx, sdk.CustomAnchorCommitOptions{})
	if err != nil {
		return nil, err
	}
	pkg, err = assetkit.FinalizeScriptSpend(ctx, f.cfg.Signer,
		s.Deposit.Key, pkg, nil)
	if err != nil {
		return nil, err
	}
	raw, err = assetkit.LogAndExport(ctx, cfg.Wallet, cfg.Chain,
		cfg.MinConfs, pkg, false)
	if err != nil {
		return nil, err
	}
	err = assetkit.VerifyOutcome(ctx, cfg.Wallet, cfg.Chain, cfg.MinConfs,
		s, raw, true, true)
	return raw, err
}
