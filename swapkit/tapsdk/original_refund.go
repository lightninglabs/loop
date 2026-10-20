package tapsdk

import (
	"context"
	"encoding/json"
	"math"

	"github.com/lightninglabs/loop/swapkit"
)

// originalRefundPolicy is also usable by a server that exposed its partial
// signature but never received the client's fully signed fallback package.
func originalRefundPolicy(d *depositRecord) (depositContract,
	*SwapAnchorSpendInfo, error) {

	var c depositContract
	if err := json.Unmarshal(d.Deposit.Contract, &c); err != nil {
		return c, nil, err
	}
	policy, err := NewDepositKit(DepositKitParams{
		ClientKey: c.ClientKey, ServerKey: c.ServerKey,
		CSVExpiry: c.CSV,
	})
	if err != nil {
		return c, nil, err
	}
	spend, err := policy.TimeoutSpend(c.Output.TaprootAssetRoot)
	return c, spend, err
}

// verifyOriginalRefund recognizes the independent original-deposit CSV path.
// The client's recovered full amount must remain unspent at its wallet key.
func (r *Driver) verifyOriginalRefund(ctx context.Context, s *swapContext,
	raw []byte) error {

	return r.verifyOriginalRefundPolicy(ctx, s, raw, true)
}

func (r *Driver) verifyOriginalRefundPolicy(ctx context.Context,
	s *swapContext, raw []byte, requireUnspent bool) error {

	d, err := r.offerDeposit(ctx, s.Offer, false)
	if err != nil {
		return err
	}
	base, err := lastProof(d.Deposit.Proof)
	if err != nil {
		return err
	}
	c, spend, err := originalRefundPolicy(d)
	if err != nil {
		return err
	}
	err = r.verifyWalletSpend(ctx, raw, c.Output, c.ClientKey,
		spend, requireUnspent, uint64(base.BlockHeight)+uint64(c.CSV))
	return err
}

// discoverOriginalRefund rebuilds an offline client's original CSV proof
// from the admitted deposit and its observed spend. No transaction is signed
// or published: only the already-mined witness is applied to the SDK package.
func (r *Driver) discoverOriginalRefund(ctx context.Context,
	s *swapContext) ([]byte, error) {

	d, err := r.offerDeposit(ctx, s.Offer, false)
	if err != nil {
		return nil, err
	}
	base, err := lastProof(d.Deposit.Proof)
	if err != nil {
		return nil, err
	}
	c, spend, err := originalRefundPolicy(d)
	if err != nil {
		return nil, err
	}
	start := uint64(base.BlockHeight) + uint64(c.CSV)
	if start > math.MaxUint32 {
		return nil, swapkit.ErrInvalid
	}
	block, _, err := r.cfg.Chain.Spend(ctx, base.OutPoint(),
		base.AnchorTx.TxOut[base.OutPoint().Index].PkScript, uint32(start))
	if err != nil {
		return nil, err
	}
	tx := spendingTransaction(block, base.OutPoint())
	if len(tx.TxIn) != 1 || len(tx.TxOut) != 1 {
		return nil, swapkit.ErrPending
	}
	if !spendsLeaf(tx, c.Output, spend) {
		return nil, swapkit.ErrPending
	}
	raw, err := r.rebuildObservedSpend(ctx, c.Output, d.Deposit.Proof,
		c.ClientKey, spend, tx)
	if err != nil {
		return nil, err
	}
	if err := r.verifyOriginalRefundPolicy(ctx, s, raw, false); err != nil {
		return nil, err
	}
	return raw, nil
}

// verifyRefundOutcome lets the server close a failed execution after the
// client has spent its returned assets. That later spend cannot restore the
// server's claim rights. Client RecoverRefund still requires spendability.
func (r *Driver) verifyRefundOutcome(ctx context.Context, s *swapContext,
	raw []byte) error {

	if err := r.verifyOutcomePolicy(ctx, s, raw, true, false); err == nil {
		return nil
	}
	return r.verifyOriginalRefundPolicy(ctx, s, raw, false)
}
