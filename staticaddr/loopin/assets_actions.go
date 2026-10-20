package loopin

import (
	"context"
	"time"

	"github.com/lightninglabs/loop/staticaddr/loopin/assetclient"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightninglabs/loop/swapserverrpc"
)

type runtimeActions struct {
	manager *Manager
}

func (a *runtimeActions) CurrentHeight(ctx context.Context) (uint32, error) {
	var height uint32
	err := a.manager.assetCfg.Runtime.Call(
		ctx,
		bridge.Height,
		struct{}{},
		&height,
	)
	return height, err
}

func (a *runtimeActions) ValidateOffer(ctx context.Context,
	o assetexec.Offer) error {

	req := bridge.Operation{Offer: o}
	return a.manager.assetCfg.Runtime.Call(ctx, bridge.ValidateOffer, req, nil)
}

func (a *runtimeActions) VerifyFinalizeFallback(ctx context.Context,
	o assetexec.Offer, bundle []byte) (assetexec.Fallback, error) {

	req := bridge.Operation{Offer: o}
	req.Bundle = bridge.Bundle{Data: bundle}
	var result bridge.Fallback
	err := a.manager.assetCfg.Runtime.Call(
		ctx, bridge.VerifyFinalizeFallback, req, &result,
	)
	return result, err
}

func (a *runtimeActions) LookupPreimage(ctx context.Context,
	o assetexec.Offer) ([32]byte, bool, error) {

	req := bridge.Operation{Offer: o}
	record, err := a.manager.assetCfg.Store.Get(ctx, o.Hash)
	if err != nil {
		return [32]byte{}, false, err
	}
	req.Fallback = record.Fallback
	var result bridge.PreimageResult
	err = a.manager.assetCfg.Runtime.Call(
		ctx,
		bridge.LookupPreimage,
		req,
		&result,
	)
	return result.Preimage, result.Found, err
}

func (a *runtimeActions) SignCooperative(ctx context.Context, o assetexec.Offer,
	request []byte) ([]byte, error) {

	req := bridge.Operation{Offer: o}
	req.Request = request
	var signature []byte
	err := a.manager.assetCfg.Runtime.Call(
		ctx,
		bridge.SignCooperative,
		req,
		&signature,
	)
	return signature, err
}

func (a *runtimeActions) InspectClaim(ctx context.Context, o assetexec.Offer,
	fallback assetexec.Fallback) ([]byte, error) {

	req := bridge.Operation{Offer: o}
	req.Fallback = fallback
	// Server proofs are optional hints. Reserve most of the action budget
	// for independent chain discovery if either retrieval or validation
	// stalls, or if the peer supplies an invalid or stale proof.
	remoteCtx, cancel := claimHintContext(ctx)
	status, remoteErr := a.manager.assetCfg.Server.GetAssetLoopIn(remoteCtx,
		&swapserverrpc.AssetLoopInReference{PaymentHash: o.Hash[:]})
	cancel()
	var proof []byte
	if remoteErr == nil && status.GetClaimed() &&
		len(status.GetResolutionProof()) > 0 {

		req.Evidence = status.GetResolutionProof()
		verifyCtx, cancel := claimHintContext(ctx)
		err := a.manager.assetCfg.Runtime.Call(
			verifyCtx, bridge.InspectClaim, req, &proof,
		)
		cancel()
		if err == nil && len(proof) > 0 {
			return proof, nil
		}
	}
	req.Evidence = nil
	proof = nil
	err := a.manager.assetCfg.Runtime.Call(ctx, bridge.InspectClaim, req, &proof)
	return proof, err
}

// claimHintContext bounds optional remote proof work without spending the
// caller's remaining chain-recovery budget.
func claimHintContext(ctx context.Context) (context.Context,
	context.CancelFunc) {

	budget := time.Second
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/4)
	}
	return context.WithTimeout(ctx, budget)
}

func (a *runtimeActions) RecoverRefund(ctx context.Context, o assetexec.Offer,
	fallback assetexec.Fallback) ([]byte, error) {

	req := bridge.Operation{Offer: o}
	req.Fallback = fallback
	var proof []byte
	if err := a.manager.assetCfg.Runtime.Call(
		ctx, bridge.RecoverRefund, req, &proof,
	); err != nil {
		return nil, err
	}
	if len(proof) > 0 {
		remoteCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, _ = a.manager.assetCfg.Server.ReportAssetLoopInRefund(remoteCtx,
			&swapserverrpc.ReportAssetLoopInRefundRequest{
				PaymentHash: o.Hash[:], Proof: proof,
			})
		cancel()
	}
	return proof, nil
}

var _ assetclient.ClientChainActions = (*runtimeActions)(nil)
