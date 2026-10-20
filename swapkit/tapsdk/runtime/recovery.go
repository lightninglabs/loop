package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	sdk "github.com/lightninglabs/tap-sdk"
)

func (r *Runtime) validateExecution(ctx context.Context,
	op bridge.Operation) error {

	d, err := r.offerDeposit(ctx, op.Offer, false)
	if err != nil {
		return err
	}
	p, err := lastProof(d.Deposit.Proof)
	if err != nil {
		return err
	}
	available, err := r.cfg.Chain.Unspent(ctx, p.OutPoint())
	if err != nil || available {
		return err
	}
	s, err := r.swap(op.Offer)
	if err != nil {
		return err
	}
	var claimed transaction
	if bridge.Decode(op.Claim.Data, &claimed) != nil ||
		!equal(claimed, s.Fallback) {
		return bridge.ErrInvalid
	}
	if _, err := finalTx(s.Fallback); err != nil {
		return err
	}
	point := sdkPoint(s.Fallback.Package.Outputs[0].AnchorOutpoint)
	available, err = r.cfg.Chain.Unspent(ctx, point)
	if err != nil {
		return err
	}
	if !available {
		return bridge.ErrPending
	}
	return nil
}

func walletOutput(public [33]byte, family int32, index uint32,
	ref sdk.AssetRef, amount uint64, value int64) (
	sdk.CustomAssetOutput, error) {

	pub, err := btcec.ParsePubKey(public[:])
	if err != nil {
		return sdk.CustomAssetOutput{}, err
	}
	tweaked := txscript.ComputeTaprootKeyNoScript(pub)
	even, err := schnorr.ParsePubKey(schnorr.SerializePubKey(tweaked))
	if err != nil {
		return sdk.CustomAssetOutput{}, err
	}
	scriptKey, err := sdk.ParsePubKey(even.SerializeCompressed())
	if err != nil {
		return sdk.CustomAssetOutput{}, err
	}
	loc := sdk.KeyLocator{Family: uint32(family), Index: index}
	return sdk.CustomAssetOutput{
		ID: "recipient", AssetRef: ref, Amount: amount,
		AnchorValueSat: uint64(value),
		Script: sdk.CustomAssetScriptPlan{
			Mode: sdk.CustomAssetScriptExternal,
			External: &sdk.CustomAssetExternalScriptPlan{
				ScriptKey: sdk.ScriptKey{PubKey: scriptKey,
					KeyDesc: sdk.KeyDescriptor{
						RawKeyBytes: public,
						KeyLocator:  loc,
					}},
			},
		},
		Anchor: sdk.CustomAnchorOutputPlan{
			InternalKey: sdk.InternalKey{
				PubKey: public, KeyLocator: loc,
			},
		},
	}, nil
}

// recoverClaim uses the client-authorized fallback, then only the server's
// wallet key and execution preimage. It never needs an online client signer.
func (r *Runtime) recoverClaim(ctx context.Context,
	op bridge.Operation) (bridge.Resolution, error) {

	if sha256.Sum256(op.Preimage[:]) != op.Offer.Hash {
		return bridge.Resolution{}, bridge.ErrInvalid
	}
	s, err := r.swap(op.Offer)
	if err != nil {
		return bridge.Resolution{}, err
	}
	var claimed transaction
	if bridge.Decode(op.Claim.Data, &claimed) != nil ||
		!equal(claimed, s.Fallback) {
		return bridge.Resolution{}, bridge.ErrInvalid
	}
	raw, err := r.recoverSpend(ctx, s, false, op.Preimage)
	return bridge.Resolution{Claimed: err == nil, Evidence: raw}, err
}

func (r *Runtime) recoverRefund(ctx context.Context,
	op bridge.Operation) ([]byte, error) {

	s, err := r.swap(op.Offer)
	if err != nil {
		return nil, err
	}
	var fallback transaction
	if bridge.Decode(op.Fallback.Bundle, &fallback) != nil ||
		!equal(fallback, s.Fallback) {
		return nil, bridge.ErrInvalid
	}
	height, err := r.cfg.Chain.Height(ctx)
	if err != nil {
		return nil, err
	}
	if height < op.Offer.Terms.RefundHeight {
		return nil, bridge.ErrPending
	}
	return r.recoverSpend(ctx, s, true, [32]byte{})
}

func (r *Runtime) recoverSpend(ctx context.Context, s *swapRecord,
	refund bool, preimage [32]byte) ([]byte, error) {

	d, err := r.offerDeposit(ctx, s.Offer, false)
	if err != nil {
		return nil, err
	}
	var c depositContract
	if err := json.Unmarshal(d.Deposit.Contract, &c); err != nil {
		return nil, err
	}
	if err := r.declareOPTrue(ctx, c.Output.ScriptKey,
		c.Output.OPTrueSpend.WitnessStack()); err != nil {
		return nil, err
	}
	fallbackProof, err := r.publish(ctx, s.Fallback, s.Since)
	if err != nil {
		return nil, err
	}
	policy, err := htlc(s.Offer, c)
	if err != nil {
		return nil, err
	}
	out := s.Fallback.Package.Outputs[0]
	plan, transactionPtr, proofPtr := &s.ClaimPlan, &s.Claim, &s.ClaimProof
	public := c.ServerKey
	spend, err := policy.SuccessSpend(out.TaprootAssetRoot)
	if refund {
		plan, transactionPtr = &s.RefundPlan, &s.Refund
		proofPtr = &s.RefundProof
		public = c.ClientKey
		spend, err = policy.TimeoutSpend(out.TaprootAssetRoot)
	}
	if err != nil {
		return nil, err
	}
	key := locator(d.Request.KeyFamily, d.Request.KeyIndex)
	if *plan == nil {
		*plan, err = r.recoveryPlan(ctx, out, fallbackProof, public,
			key, spend)
		if err != nil {
			return nil, err
		}
		if err := r.save(s); err != nil {
			return nil, err
		}
	}
	var unlock *[32]byte
	if !refund {
		unlock = &preimage
	}
	err = r.signScriptSpend(ctx, *plan, transactionPtr, key, unlock,
		func() error { return r.save(s) })
	if err != nil {
		return nil, err
	}
	raw, err := r.publish(ctx, *transactionPtr, s.Since)
	if err != nil {
		return nil, err
	}
	if err := r.verifyOutcome(ctx, s, raw, refund); err != nil {
		return nil, err
	}
	_, err = r.cfg.Wallet.ImportProofFile(ctx,
		&sdk.ProofFile{RawProofFile: raw})
	if err != nil {
		return nil, err
	}
	*proofPtr = raw
	if err := r.save(s); err != nil {
		return nil, err
	}
	return raw, nil
}

func (r *Runtime) inspectClaim(ctx context.Context,
	op bridge.Operation) ([]byte, error) {

	s, err := r.swap(op.Offer)
	if err != nil {
		return nil, err
	}
	raw, err := resolveOutcomeProof(ctx, op.Evidence, s.ClaimProof,
		func(ctx context.Context, raw []byte) error {
			return r.verifyOutcome(ctx, s, raw, false)
		}, func(ctx context.Context) ([]byte, error) {
			return r.discoverOutcome(ctx, s, false)
		})
	if err != nil {
		return nil, err
	}
	s.ClaimProof = raw
	return raw, r.save(s)
}

func (r *Runtime) inspectRefund(ctx context.Context,
	op bridge.Operation) (bridge.Resolution, error) {

	s, err := r.swap(op.Offer)
	if err != nil {
		return bridge.Resolution{}, err
	}
	raw, err := resolveOutcomeProof(ctx, op.Evidence, s.RefundProof,
		func(ctx context.Context, raw []byte) error {
			return r.verifyRefundOutcome(ctx, s, raw)
		}, func(ctx context.Context) ([]byte, error) {
			raw, err := r.discoverOutcome(ctx, s, true)
			if err == bridge.ErrPending {
				return r.discoverOriginalRefund(ctx, s)
			}
			return raw, err
		})
	if err == bridge.ErrPending {
		return bridge.Resolution{}, nil
	}
	if err != nil {
		return bridge.Resolution{}, err
	}
	s.RefundProof = raw
	if err := r.save(s); err != nil {
		return bridge.Resolution{}, err
	}
	return bridge.Resolution{Refunded: true, Evidence: raw}, nil
}

// discoverOutcome rebuilds a missing peer proof from the known fallback and
// its confirmed spend. OP_TRUE makes the asset transition deterministic;
// the observed Bitcoin signature is copied only after checking the contract.
// This remains usable when the claiming/refunding peer is offline forever.
func (r *Runtime) discoverOutcome(ctx context.Context, s *swapRecord,
	refund bool) ([]byte, error) {

	if s.Fallback.Package == nil {
		return nil, bridge.ErrPending
	}
	d, err := r.offerDeposit(ctx, s.Offer, false)
	if err != nil {
		return nil, err
	}
	var c depositContract
	if err := json.Unmarshal(d.Deposit.Contract, &c); err != nil {
		return nil, err
	}
	out := s.Fallback.Package.Outputs[0]
	point := sdkPoint(out.AnchorOutpoint)
	block, height, err := r.findSpend(ctx, point, s.Since)
	if err != nil {
		return nil, err
	}
	found := spendingTransaction(block, point)
	if len(found.TxIn) != 1 || len(found.TxOut) != 1 {
		return nil, bridge.ErrInvalid
	}
	policy, err := htlc(s.Offer, c)
	if err != nil {
		return nil, err
	}
	public := c.ServerKey
	spend, err := policy.SuccessSpend(out.TaprootAssetRoot)
	if refund {
		public = c.ClientKey
		spend, err = policy.TimeoutSpend(out.TaprootAssetRoot)
	}
	if err != nil {
		return nil, err
	}
	if !spendsLeaf(found, out, spend) {
		return nil, bridge.ErrPending
	}
	fallbackProof, err := r.publish(ctx, s.Fallback, s.Since)
	if err != nil {
		return nil, err
	}
	raw, err := r.rebuildObservedSpend(ctx, out, fallbackProof, public,
		spend, found, block, height)
	if err != nil {
		return nil, err
	}
	err = r.verifyOutcomePolicy(ctx, s, raw, refund, false)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// verifyOutcome requires the agreed asset lineage, the authorized recipient,
// a currently confirmed Bitcoin spend, and the exact success/refund leaf.
// Client refund recovery additionally requires the output to remain unspent.
func (r *Runtime) verifyOutcome(ctx context.Context, s *swapRecord,
	raw []byte, refund bool) error {

	return r.verifyOutcomePolicy(ctx, s, raw, refund, refund)
}

func (r *Runtime) verifyOutcomePolicy(ctx context.Context, s *swapRecord,
	raw []byte, refund, requireUnspent bool) error {

	d, err := r.offerDeposit(ctx, s.Offer, false)
	if err != nil {
		return err
	}
	var c depositContract
	if err := json.Unmarshal(d.Deposit.Contract, &c); err != nil {
		return err
	}
	if s.Fallback.Package == nil {
		return bridge.ErrPending
	}
	prior := s.Fallback.Package.Outputs[0]
	if prior.AssetRef != sdk.AssetRefFromGroupKey(s.Offer.Terms.GroupKey) ||
		prior.Amount != s.Offer.Terms.DepositAmount {

		return bridge.ErrInvalid
	}
	pub := c.ServerKey
	if refund {
		pub = c.ClientKey
	}
	policy, err := htlc(s.Offer, c)
	if err != nil {
		return err
	}
	spend, err := policy.SuccessSpend(prior.TaprootAssetRoot)
	if refund {
		spend, err = policy.TimeoutSpend(prior.TaprootAssetRoot)
	}
	if err != nil {
		return err
	}
	err = r.verifyWalletSpend(ctx, raw, prior, pub, spend,
		requireUnspent, 0)
	return err
}

func (r *Runtime) lookupPreimage(ctx context.Context,
	op bridge.Operation) (bridge.PreimageResult, error) {

	s, err := r.swap(op.Offer)
	if err != nil {
		return bridge.PreimageResult{}, err
	}
	if s.Fallback.Package == nil {
		return bridge.PreimageResult{}, nil
	}
	_, err = r.offerDeposit(ctx, op.Offer, false)
	if err != nil {
		return bridge.PreimageResult{}, err
	}
	point := sdkPoint(s.Fallback.Package.Outputs[0].AnchorOutpoint)
	block, _, err := r.findSpend(ctx, point, s.Since)
	if err == bridge.ErrPending {
		return bridge.PreimageResult{}, nil
	}
	if err != nil {
		return bridge.PreimageResult{}, err
	}
	for _, input := range spendingTransaction(block, point).TxIn {
		if input.PreviousOutPoint != point {
			continue
		}
		for _, element := range input.Witness {
			if len(element) != 32 || sha256.Sum256(element) != op.Offer.Hash {
				continue
			}
			var preimage [32]byte
			copy(preimage[:], element)
			return bridge.PreimageResult{Preimage: preimage, Found: true}, nil
		}
	}
	return bridge.PreimageResult{}, nil
}
