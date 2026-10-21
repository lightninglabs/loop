package tapsdk

import (
	"context"
	"encoding/json"

	"github.com/lightninglabs/loop/swapkit"
)

// BuildDepositRefund signs the original deposit CSV path without publishing.
// The deposit FSM checks ownership and saves the result before publication.
func (r *Driver) BuildDepositRefund(ctx context.Context,
	deposit swapkit.Deposit) ([]byte, error) {

	d, err := r.localDeposit(ctx, deposit.Outpoint)
	if err != nil {
		return nil, err
	}
	if !equal(d.Deposit, deposit) {
		return nil, swapkit.ErrInvalid
	}
	p, err := r.verifyDeposit(ctx, deposit, true)
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
	plan, err := r.recoveryPlan(ctx, c.Output, deposit.Proof, c.ClientKey,
		locator(d.Request.KeyFamily, d.Request.KeyIndex), spend,
		r.cfg.FeeSat)
	if err != nil {
		return nil, err
	}
	var tx transaction
	if err := r.signScriptSpend(ctx, plan, &tx,
		locator(d.Request.KeyFamily, d.Request.KeyIndex), nil); err != nil {
		return nil, err
	}
	return json.Marshal(tx)
}

// PublishDepositRefund publishes the deposit FSM's saved CSV spend and verifies
// its proof. Retrying publication never changes the signed transaction.
func (r *Driver) PublishDepositRefund(ctx context.Context,
	deposit swapkit.Deposit, signedPackage []byte) ([]byte, error) {

	d, err := r.localDeposit(ctx, deposit.Outpoint)
	if err != nil {
		return nil, err
	}
	if !equal(d.Deposit, deposit) {
		return nil, swapkit.ErrInvalid
	}
	p, err := r.verifyDeposit(ctx, deposit, false)
	if err != nil {
		return nil, err
	}
	c, spend, err := originalRefundPolicy(d)
	if err != nil {
		return nil, err
	}
	var tx transaction
	if swapkit.Decode(signedPackage, &tx) != nil {
		return nil, swapkit.ErrInvalid
	}
	raw, err := r.publish(ctx, tx)
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
