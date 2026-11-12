package tapsdk

import (
	"context"
	"math"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

// OriginalRefundPolicy selects the original deposit's unilateral CSV leaf.
func OriginalRefundPolicy(d *DepositContext) (DepositContract,
	*swapkit.SwapAnchorSpendInfo, error) {

	c := d.Contract
	policy, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: c.ClientKey, ServerKey: c.ServerKey,
		CSVExpiry: c.CSV,
	})
	if err != nil {
		return c, nil, err
	}
	spend, err := policy.TimeoutSpend(c.Output.TaprootAssetRoot)
	return c, spend, err
}

// verifyOriginalRefund verifies the original deposit's CSV recovery proof.
func verifyOriginalRefund(ctx context.Context, wallet *sdk.Wallet,
	chain Chain, minConfs uint32,
	s *SwapContext, raw []byte, requireUnspent bool) error {

	d := s.Deposit
	base := d.Proof
	c, spend, err := OriginalRefundPolicy(d)
	if err != nil {
		return err
	}
	err = VerifyWalletSpend(
		ctx, wallet, chain, minConfs, raw, c.Output, c.ClientKey,
		spend, requireUnspent, uint64(base.BlockHeight)+uint64(c.CSV),
	)
	return err
}

// DiscoverOriginalRefund reconstructs an offline client's CSV recovery proof.
func DiscoverOriginalRefund(ctx context.Context, wallet *sdk.Wallet,
	chain Chain, minConfs uint32,
	s *SwapContext) ([]byte, error) {

	d := s.Deposit
	base := d.Proof
	c, spend, err := OriginalRefundPolicy(d)
	if err != nil {
		return nil, err
	}
	start := uint64(base.BlockHeight) + uint64(c.CSV)
	if start > math.MaxUint32 {
		return nil, swapkit.ErrInvalid
	}
	block, _, err := chain.Spend(
		ctx,
		base.OutPoint(),
		base.AnchorTx.TxOut[base.OutPoint().Index].PkScript,
		uint32(start),
	)
	if err != nil {
		return nil, err
	}
	tx := spendingTransaction(block, base.OutPoint())
	if tx == nil {
		// A faithful Chain returns a block that spends the point, but a
		// stale or inconsistent response must not panic the recovery.
		return nil, swapkit.ErrPending
	}
	// A confirmed spend with extra inputs or outputs cannot be rebuilt into
	// a single-input recovery proof and never will be, so it is terminal.
	// DiscoverOutcome classifies the same shape identically.
	if len(tx.TxIn) != 1 || len(tx.TxOut) != 1 {
		return nil, swapkit.ErrInvalid
	}
	if !spendsLeaf(tx, c.Output, spend) {
		return nil, swapkit.ErrPending
	}
	raw, err := rebuildObservedSpend(
		ctx, wallet, chain, minConfs, c.Output, d.Deposit.Proof,
		c.ClientKey, spend, tx,
	)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// VerifyRefundOutcome recognizes either refund after it is spent again.
// A later wallet spend cannot restore the server's expired claim authority.
func VerifyRefundOutcome(ctx context.Context, wallet *sdk.Wallet,
	chain Chain, minConfs uint32, s *SwapContext,
	raw []byte) error {

	p, err := LastProof(raw)
	if err != nil {
		return err
	}
	if len(p.AnchorTx.TxIn) == 1 && p.AnchorTx.TxIn[0].PreviousOutPoint ==
		s.Deposit.Proof.OutPoint() {

		return verifyOriginalRefund(
			ctx,
			wallet,
			chain,
			minConfs,
			s,
			raw,
			false,
		)
	}
	return VerifyOutcome(ctx, wallet, chain, minConfs, s, raw, true, false)
}
