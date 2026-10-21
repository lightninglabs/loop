package tapsdk

import (
	"context"
	"crypto/sha256"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

func (r *Driver) ValidateExecution(ctx context.Context,
	op swapkit.Operation) error {

	if len(op.Claim.Data) == 0 {
		return swapkit.ErrInvalid
	}
	s, err := r.swap(ctx, op)
	if err != nil {
		return err
	}
	p := s.Deposit.Proof
	available, err := r.cfg.Chain.Unspent(ctx, p.OutPoint())
	if err != nil || available {
		return err
	}
	point := sdkPoint(s.Fallback.Outputs[0].AnchorOutpoint)
	available, err = r.cfg.Chain.Unspent(ctx, point)
	if err != nil {
		return err
	}
	if !available {
		return swapkit.ErrPending
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

// RecoverClaim uses the client-authorized fallback, then only the server's
// wallet key and execution preimage. It never needs an online client signer.
func (r *Driver) RecoverClaim(ctx context.Context,
	op swapkit.Operation) (swapkit.Resolution, error) {

	if sha256.Sum256(op.Preimage[:]) != op.Offer.Hash {
		return swapkit.Resolution{}, swapkit.ErrInvalid
	}
	s, err := r.swap(ctx, op)
	if err != nil {
		return swapkit.Resolution{}, err
	}
	if len(op.Claim.Data) == 0 {
		return swapkit.Resolution{}, swapkit.ErrInvalid
	}
	raw, err := r.recoverSpend(ctx, s, false, op.Preimage)
	return swapkit.Resolution{Claimed: err == nil, Evidence: raw}, err
}

func (r *Driver) RecoverRefund(ctx context.Context,
	op swapkit.Operation) ([]byte, error) {

	s, err := r.swap(ctx, op)
	if err != nil {
		return nil, err
	}
	if len(op.Fallback.Bundle) == 0 {
		return nil, swapkit.ErrInvalid
	}
	height, err := r.cfg.Chain.Height(ctx)
	if err != nil {
		return nil, err
	}
	if height < op.Offer.Terms.RefundHeight {
		return nil, swapkit.ErrPending
	}
	return r.recoverSpend(ctx, s, true, [32]byte{})
}

func (r *Driver) recoverSpend(ctx context.Context, s *swapContext,
	refund bool, preimage [32]byte) ([]byte, error) {

	d := s.Deposit
	c := d.Contract
	fallbackProof, err := r.fallbackProof(ctx, s, false)
	if err != nil {
		return nil, err
	}
	policy, err := htlc(s.Offer, c)
	if err != nil {
		return nil, err
	}
	out := s.Fallback.Outputs[0]
	public := c.ServerKey
	spend, err := policy.SuccessSpend(out.TaprootAssetRoot)
	if refund {
		public = c.ClientKey
		spend, err = policy.TimeoutSpend(out.TaprootAssetRoot)
	}
	if err != nil {
		return nil, err
	}
	// Keys and the fee are fixed by the agreed fallback. Rebuilding after
	// restart therefore has the same unsigned transaction and asset output.
	fee := c.Output.AnchorValueSat - out.AnchorValueSat
	if fee <= 0 {
		return nil, swapkit.ErrInvalid
	}
	key := locator(d.Request.KeyFamily, d.Request.KeyIndex)
	plan, err := r.recoveryPlan(ctx, out, fallbackProof, public, key, spend,
		uint64(fee))
	if err != nil {
		return nil, err
	}
	var unlock *[32]byte
	if !refund {
		unlock = &preimage
	}
	pkg, err := r.commit(ctx, plan)
	if err != nil {
		return nil, err
	}
	pkg, err = r.signScriptSpend(ctx, pkg, key, unlock)
	if err != nil {
		return nil, err
	}
	raw, err := r.publish(ctx, pkg)
	if err != nil {
		return nil, err
	}
	if err := r.verifyOutcome(ctx, s, raw, refund); err != nil {
		return nil, err
	}
	return raw, nil
}

func (r *Driver) InspectClaim(ctx context.Context,
	op swapkit.Operation) ([]byte, error) {

	s, err := r.swap(ctx, op)
	if err != nil {
		return nil, err
	}
	raw, err := resolveOutcomeProof(ctx, op.Evidence,
		func(ctx context.Context, raw []byte) error {
			return r.verifyOutcome(ctx, s, raw, false)
		}, func(ctx context.Context) ([]byte, error) {
			return r.discoverOutcome(ctx, s, false)
		})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (r *Driver) InspectRefund(ctx context.Context,
	op swapkit.Operation) (swapkit.Resolution, error) {

	s, err := r.swap(ctx, op)
	if err != nil {
		return swapkit.Resolution{}, err
	}
	raw, err := resolveOutcomeProof(ctx, op.Evidence,
		func(ctx context.Context, raw []byte) error {
			return r.verifyRefundOutcome(ctx, s, raw)
		}, func(ctx context.Context) ([]byte, error) {
			raw, err := r.discoverOutcome(ctx, s, true)
			if err == swapkit.ErrPending {
				return r.discoverOriginalRefund(ctx, s)
			}
			return raw, err
		})
	if err == swapkit.ErrPending {
		return swapkit.Resolution{}, nil
	}
	if err != nil {
		return swapkit.Resolution{}, err
	}
	return swapkit.Resolution{Refunded: true, Evidence: raw}, nil
}

// discoverOutcome rebuilds a missing peer proof from the known fallback and
// its confirmed spend. OP_TRUE makes the asset transition deterministic;
// the observed Bitcoin signature is copied only after checking the contract.
// This remains usable when the claiming/refunding peer is offline forever.
func (r *Driver) discoverOutcome(ctx context.Context, s *swapContext,
	refund bool) ([]byte, error) {

	if s.Fallback == nil {
		return nil, swapkit.ErrPending
	}
	d := s.Deposit
	c := d.Contract
	out := s.Fallback.Outputs[0]
	point := sdkPoint(out.AnchorOutpoint)
	// The server may only have its public partial after a lost reply.
	// The committed unsigned anchor still fixes the output to watch.
	anchor, err := decodePSBT(s.Fallback.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	block, _, err := r.cfg.Chain.Spend(ctx, point,
		anchor.UnsignedTx.TxOut[point.Index].PkScript, s.Deposit.Proof.BlockHeight)
	if err != nil {
		return nil, err
	}
	found := spendingTransaction(block, point)
	if len(found.TxIn) != 1 || len(found.TxOut) != 1 {
		return nil, swapkit.ErrInvalid
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
		return nil, swapkit.ErrPending
	}
	fallbackProof, err := r.fallbackProof(ctx, s, true)
	if err != nil {
		return nil, err
	}
	raw, err := r.rebuildObservedSpend(ctx, out, fallbackProof, public,
		spend, found)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// verifyOutcome requires the agreed asset lineage, the authorized recipient,
// a currently confirmed Bitcoin spend, and the exact success/refund leaf.
// Client refund recovery additionally requires the output to remain unspent.
func (r *Driver) verifyOutcome(ctx context.Context, s *swapContext,
	raw []byte, refund bool) error {

	return r.verifyOutcomePolicy(ctx, s, raw, refund, refund)
}

func (r *Driver) verifyOutcomePolicy(ctx context.Context, s *swapContext,
	raw []byte, refund, requireUnspent bool) error {

	d := s.Deposit
	c := d.Contract
	if s.Fallback == nil {
		return swapkit.ErrPending
	}
	prior := s.Fallback.Outputs[0]
	if prior.AssetRef != sdk.AssetRefFromGroupKey(s.Offer.Terms.GroupKey) ||
		prior.Amount != s.Offer.Terms.DepositAmount {

		return swapkit.ErrInvalid
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

func (r *Driver) LookupPreimage(ctx context.Context,
	op swapkit.Operation) (swapkit.PreimageResult, error) {

	s, err := r.swap(ctx, op)
	if err != nil {
		return swapkit.PreimageResult{}, err
	}
	if s.Fallback == nil {
		return swapkit.PreimageResult{}, nil
	}
	point := sdkPoint(s.Fallback.Outputs[0].AnchorOutpoint)
	anchor, err := decodePSBT(s.Fallback.AnchorPsbt)
	if err != nil {
		return swapkit.PreimageResult{}, err
	}
	block, _, err := r.cfg.Chain.Spend(ctx, point,
		anchor.UnsignedTx.TxOut[point.Index].PkScript, s.Deposit.Proof.BlockHeight)
	if err == swapkit.ErrPending {
		return swapkit.PreimageResult{}, nil
	}
	if err != nil {
		return swapkit.PreimageResult{}, err
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
			return swapkit.PreimageResult{Preimage: preimage, Found: true}, nil
		}
	}
	return swapkit.PreimageResult{}, nil
}

// fallbackProof recovers a complete fallback even if the client's final reply
// was lost: the fixed unsigned transaction identifies its observed witness.
// A confirmed successor also proves the fallback was mined; importing that
// lineage must not try to broadcast its already-spent anchor.
func (r *Driver) fallbackProof(ctx context.Context,
	s *swapContext, observed bool) ([]byte, error) {

	if s.Fallback == nil {
		return nil, swapkit.ErrPending
	}
	contract := s.Deposit.Contract
	if contract.Output.OPTrueSpend == nil {
		return nil, swapkit.ErrInvalid
	}
	if err := r.declareOPTrue(ctx, contract.Output.ScriptKey,
		contract.Output.OPTrueSpend.WitnessStack()); err != nil {
		return nil, err
	}
	pkg := s.Fallback
	packet, err := decodePSBT(pkg.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	if !packet.IsComplete() {
		observed = true
		if len(packet.UnsignedTx.TxIn) != 1 || len(packet.Inputs) != 1 ||
			packet.Inputs[0].WitnessUtxo == nil {

			return nil, swapkit.ErrInvalid
		}
		point := packet.UnsignedTx.TxIn[0].PreviousOutPoint
		block, _, err := r.cfg.Chain.Spend(
			ctx, point, packet.Inputs[0].WitnessUtxo.PkScript,
			s.Deposit.Proof.BlockHeight,
		)
		if err != nil {
			return nil, err
		}
		tx := spendingTransaction(block, point)
		if tx == nil || tx.TxHash() != packet.UnsignedTx.TxHash() ||
			len(tx.TxIn[0].Witness) != 1 {

			return nil, swapkit.ErrPending
		}
		requests, err := pkg.SigningRequests()
		if err != nil || len(requests.MuSig2) != 1 {
			return nil, swapkit.ErrInvalid
		}
		signed, err := pkg.ApplyKeyPathSignature(
			requests.MuSig2[0].ID, tx.TxIn[0].Witness[0],
		)
		if err != nil {
			return nil, err
		}
		packet, err = decodePSBT(signed.AnchorPsbt)
		if err != nil {
			return nil, err
		}
		if err := psbt.MaybeFinalizeAll(packet); err != nil {
			return nil, err
		}
		final, err := encodePSBT(packet)
		if err != nil {
			return nil, err
		}
		s.Fallback, err = pkg.WithFinalAnchorPSBT(final)
		if err != nil {
			return nil, err
		}
	}
	raw, err := r.logAndExport(ctx, s.Fallback, observed)
	if err != nil {
		return nil, err
	}
	_, err = r.verifyProof(ctx, raw, s.Offer.Terms.GroupKey,
		s.Offer.Terms.DepositAmount, false)
	return raw, err
}
