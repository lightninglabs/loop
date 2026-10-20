package runtime

import (
	"context"
	"time"

	"github.com/lightninglabs/loop/swapkit/bridge"
	sdk "github.com/lightninglabs/tap-sdk"
)

// refundDeposit implements the original static deposit CSV path without a
// server offer. The client manager separately retains pending-swap exclusions.
func (r *Runtime) refundDeposit(ctx context.Context,
	request bridge.RefundDepositRequest) ([]byte, error) {

	d, err := r.localDeposit(request.Outpoint)
	if err != nil {
		return nil, err
	}
	p, err := r.verifyDeposit(ctx, d.Deposit, false)
	if err != nil {
		return nil, err
	}
	tip, err := r.cfg.Chain.Height(ctx)
	if err != nil {
		return nil, err
	}
	if uint64(tip) < uint64(p.BlockHeight)+uint64(d.Request.CSV) {
		return nil, bridge.ErrPending
	}
	// Expired signing material is retained, never erased to make the
	// anchor appear free. Every admitted fallback expires before this CSV.
	swaps, err := records[swapRecord](r, "swap/")
	if err != nil {
		return nil, err
	}
	for _, swap := range swaps {
		terms := swap.Offer.Terms
		for _, point := range terms.DepositOutpoints {
			if point != request.Outpoint {
				continue
			}
			deadline := uint64(terms.RefundHeight) +
				uint64(terms.RecoveryDelta)
			if time.Now().Before(terms.OfferExpiry) ||
				uint64(tip) < deadline {

				return nil, bridge.ErrPending
			}
		}
	}
	c, spend, err := originalRefundPolicy(d)
	if err != nil {
		return nil, err
	}
	id := key("deposit/", d.Request.RequestID)
	if d.RefundPlan == nil {
		d.RefundSince = tip
		free, err := r.cfg.Chain.Unspent(ctx, p.OutPoint())
		if err != nil {
			return nil, err
		}
		if !free {
			return nil, bridge.ErrPending
		}
		d.RefundPlan, err = r.recoveryPlan(ctx, c.Output,
			d.Deposit.Proof, c.ClientKey,
			locator(d.Request.KeyFamily, d.Request.KeyIndex), spend)
		if err != nil {
			return nil, err
		}
		if err := r.put(id, d); err != nil {
			return nil, err
		}
	}
	err = r.signScriptSpend(ctx, d.RefundPlan, &d.Refund,
		locator(d.Request.KeyFamily, d.Request.KeyIndex), nil,
		func() error { return r.put(id, d) })
	if err != nil {
		return nil, err
	}
	raw, err := r.publish(ctx, d.Refund, d.RefundSince)
	if err != nil {
		return nil, err
	}
	err = r.verifyWalletSpend(ctx, raw, c.Output, c.ClientKey, spend,
		true, uint64(p.BlockHeight)+uint64(c.CSV))
	if err != nil {
		return nil, err
	}
	_, err = r.cfg.Wallet.ImportProofFile(ctx,
		&sdk.ProofFile{RawProofFile: raw})
	if err != nil {
		return nil, err
	}
	d.RefundProof = raw
	return raw, r.put(id, d)
}
