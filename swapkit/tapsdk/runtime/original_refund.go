package runtime

import (
	"context"
	"encoding/json"
	"math"

	"github.com/lightninglabs/loop/swapkit/bridge"
	kit "github.com/lightninglabs/loop/swapkit/tapsdk"
)

// originalRefundPolicy is also usable by a server that exposed its partial
// signature but never received the client's fully signed fallback package.
func originalRefundPolicy(d *depositRecord) (depositContract,
	*kit.SwapAnchorSpendInfo, error) {

	var c depositContract
	if err := json.Unmarshal(d.Deposit.Contract, &c); err != nil {
		return c, nil, err
	}
	policy, err := kit.NewDepositKit(kit.DepositKitParams{
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
func (r *Runtime) verifyOriginalRefund(ctx context.Context, s *swapRecord,
	raw []byte) error {

	return r.verifyOriginalRefundPolicy(ctx, s, raw, true)
}

func (r *Runtime) verifyOriginalRefundPolicy(ctx context.Context,
	s *swapRecord, raw []byte, requireUnspent bool) error {

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
func (r *Runtime) discoverOriginalRefund(ctx context.Context,
	s *swapRecord) ([]byte, error) {

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
		return nil, bridge.ErrInvalid
	}
	block, height, err := r.findSpend(ctx, base.OutPoint(), uint32(start))
	if err != nil {
		return nil, err
	}
	tx := spendingTransaction(block, base.OutPoint())
	if len(tx.TxIn) != 1 || len(tx.TxOut) != 1 {
		return nil, bridge.ErrPending
	}
	if !spendsLeaf(tx, c.Output, spend) {
		return nil, bridge.ErrPending
	}
	raw, err := r.rebuildObservedSpend(ctx, c.Output, d.Deposit.Proof,
		c.ClientKey, spend, tx, block, height)
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
func (r *Runtime) verifyRefundOutcome(ctx context.Context, s *swapRecord,
	raw []byte) error {

	if err := r.verifyOutcomePolicy(ctx, s, raw, true, false); err == nil {
		return nil
	}
	return r.verifyOriginalRefundPolicy(ctx, s, raw, false)
}
