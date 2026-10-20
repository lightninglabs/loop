package loopin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lightninglabs/loop/staticaddr/loopin/assetclient"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/loop/swapserverrpc"
)

type assetActions struct {
	manager *Manager
}

func (a *assetActions) CurrentHeight(ctx context.Context) (uint32, error) {
	return a.manager.assetCfg.Driver.Height(ctx)
}

func (a *assetActions) ValidateOffer(ctx context.Context,
	o assetexec.Offer) error {

	return a.manager.assetCfg.Driver.ValidateOffer(ctx, o)
}

func (a *assetActions) BeginFallback(ctx context.Context, o assetexec.Offer,
	request []byte) (*assetexec.FallbackRound, error) {

	return a.manager.assetCfg.Driver.BeginFallback(ctx, o, request)
}

// operation recovers the authoritative package from the normal swap record.
func (a *assetActions) operation(ctx context.Context, o assetexec.Offer) (
	swapkit.Operation, error) {

	op := swapkit.Operation{Offer: o}
	record, err := a.manager.assetCfg.Store.Get(ctx, o.Hash)
	if err != nil {
		return op, err
	}
	var intent assetclient.InvoiceIntent
	if err := json.Unmarshal(record.InvoiceIntent, &intent); err != nil {
		return op, err
	}
	op.Preparation.RecoveryRequest = intent.RecoveryRequest
	op.Fallback = record.Fallback
	return op, nil
}

func (a *assetActions) LookupPreimage(ctx context.Context, o assetexec.Offer) (
	[32]byte, bool, error) {

	op, err := a.operation(ctx, o)
	if err != nil {
		return [32]byte{}, false, err
	}
	result, err := a.manager.assetCfg.Driver.LookupPreimage(ctx, op)
	return result.Preimage, result.Found, err
}

func (a *assetActions) SignCooperative(context.Context, assetexec.Offer,
	[]byte) ([]byte, error) {

	return nil, assetexec.ErrPending
}

func (a *assetActions) InspectClaim(ctx context.Context, o assetexec.Offer,
	fallback assetexec.Fallback) ([]byte, error) {

	req, err := a.operation(ctx, o)
	if err != nil {
		return nil, err
	}
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
		proof, err = a.manager.assetCfg.Driver.InspectClaim(verifyCtx, req)
		cancel()
		if err == nil && len(proof) > 0 {
			return proof, nil
		}
	}
	req.Evidence = nil
	return a.manager.assetCfg.Driver.InspectClaim(ctx, req)
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

func (a *assetActions) RecoverRefund(ctx context.Context, o assetexec.Offer,
	fallback assetexec.Fallback) ([]byte, error) {

	req, err := a.operation(ctx, o)
	if err != nil {
		return nil, err
	}
	req.Fallback = fallback
	proof, err := a.manager.assetCfg.Driver.RecoverRefund(ctx, req)
	if err != nil {
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

var _ assetclient.ClientChainActions = (*assetActions)(nil)
