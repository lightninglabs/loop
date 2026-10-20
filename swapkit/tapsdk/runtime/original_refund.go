package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"math"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
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
	p, err := r.verifyProof(ctx, raw, s.Offer.Terms.GroupKey,
		s.Offer.Terms.DepositAmount, requireUnspent)
	if err != nil {
		return err
	}
	if p.Asset.ID() != base.Asset.ID() ||
		uint64(p.BlockHeight) < uint64(base.BlockHeight)+uint64(c.CSV) {

		return bridge.ErrInvalid
	}
	expected, err := walletOutput(c.ClientKey, 0, 0,
		c.Output.AssetRef, c.Output.Amount, 330)
	if err != nil {
		return err
	}
	internal := p.InclusionProof.InternalKey.SerializeCompressed()
	if !bytes.Equal(p.Asset.ScriptKey.PubKey.SerializeCompressed(),
		expected.Script.External.ScriptKey.PubKey[:]) ||
		!bytes.Equal(internal, c.ClientKey[:]) {

		return bridge.ErrInvalid
	}
	block, err := r.cfg.Chain.Block(ctx, p.BlockHeight)
	if err != nil {
		return err
	}
	for _, tx := range block.Transactions {
		if tx.TxHash() != p.AnchorTx.TxHash() {
			continue
		}
		if len(tx.TxIn) != 1 ||
			tx.TxIn[0].PreviousOutPoint != base.OutPoint() {

			return bridge.ErrInvalid
		}
		witness := tx.TxIn[0].Witness
		if len(witness) != 3 ||
			!bytes.Equal(witness[1], spend.LeafScript) ||
			!bytes.Equal(witness[2], spend.ControlBlock) {

			return bridge.ErrInvalid
		}
		value := c.Output.AnchorValueSat
		fetch := txscript.NewCannedPrevOutputFetcher(
			spend.PkScript, value,
		)
		hashes := txscript.NewTxSigHashes(tx, fetch)
		engine, err := txscript.NewEngine(spend.PkScript, tx, 0,
			txscript.StandardVerifyFlags, nil, hashes, value, fetch)
		if err != nil {
			return err
		}
		return engine.Execute()
	}
	return bridge.ErrInvalid
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
	w := tx.TxIn[0].Witness
	if len(w) != 3 || !bytes.Equal(w[1], spend.LeafScript) ||
		!bytes.Equal(w[2], spend.ControlBlock) {

		return nil, bridge.ErrPending
	}
	return r.rebuildOriginalRefund(ctx, s, d, c, spend, tx, block, height)
}

func (r *Runtime) rebuildOriginalRefund(ctx context.Context, s *swapRecord,
	d *depositRecord, c depositContract, spend *kit.SwapAnchorSpendInfo,
	observed *wire.MsgTx, block *wire.MsgBlock,
	height uint32) ([]byte, error) {

	base, err := lastProof(d.Deposit.Proof)
	if err != nil {
		return nil, err
	}
	value := observed.TxOut[0].Value
	recipient, err := walletOutput(c.ClientKey, 0, 0,
		c.Output.AssetRef, c.Output.Amount, value)
	if err != nil {
		return nil, err
	}
	anchor, err := anchorTemplate(base, recipient.Anchor.InternalKey,
		value, spend)
	if err != nil {
		return nil, err
	}
	request := customRequest(assetInput(c.Output, d.Deposit.Proof),
		recipient, anchor, spend.SigningPlan(0))
	pkg, err := r.commit(ctx, request)
	if err != nil {
		return nil, err
	}
	packet, err := decodePSBT(pkg.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	if packet.UnsignedTx.TxHash() != observed.TxHash() {
		return nil, bridge.ErrInvalid
	}
	requests, err := pkg.SigningRequests()
	if err != nil || len(requests.ScriptPath) != 1 {
		return nil, bridge.ErrInvalid
	}
	final, err := pkg.ApplyScriptPathWitness(requests.ScriptPath[0].ID,
		observed.TxIn[0].Witness[:1])
	if err != nil {
		return nil, err
	}
	raw, err := confirmedProof(transaction{Package: pkg,
		Final: final.AnchorPsbt}, block, height)
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
