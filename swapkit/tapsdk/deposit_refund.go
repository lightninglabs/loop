package tapsdk

import (
	"context"

	"github.com/lightninglabs/loop/swapkit"
)

// RefundDeposit implements the original static deposit CSV path without a
// server offer. The client manager separately retains pending-swap exclusions.
func (r *Driver) RefundDeposit(ctx context.Context,
	request swapkit.RefundDepositRequest, funding *DepositFunding,
	save func() error) ([]byte, error) {

	if funding == nil || save == nil {
		return nil, swapkit.ErrInvalid
	}

	d, err := r.localDeposit(ctx, request.Outpoint)
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
		return nil, swapkit.ErrPending
	}
	c, spend, err := originalRefundPolicy(d)
	if err != nil {
		return nil, err
	}
	if funding.Plan == nil {
		free, err := r.cfg.Chain.Unspent(ctx, p.OutPoint())
		if err != nil {
			return nil, err
		}
		if !free {
			return nil, swapkit.ErrPending
		}
		funding.Plan, err = r.recoveryPlan(ctx, c.Output,
			d.Deposit.Proof, c.ClientKey,
			locator(d.Request.KeyFamily, d.Request.KeyIndex), spend,
			r.cfg.FeeSat)
		if err != nil {
			return nil, err
		}
		if err := save(); err != nil {
			return nil, err
		}
	}
	err = r.signScriptSpend(ctx, funding.Plan, &funding.Transaction,
		locator(d.Request.KeyFamily, d.Request.KeyIndex), nil,
		func() error { return save() })
	if err != nil {
		return nil, err
	}
	raw, err := r.publish(ctx, funding.Transaction)
	if err != nil {
		return nil, err
	}
	err = r.verifyWalletSpend(ctx, raw, c.Output, c.ClientKey, spend,
		true, uint64(p.BlockHeight)+uint64(c.CSV))
	if err != nil {
		return nil, err
	}
	return raw, nil
}
