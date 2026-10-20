package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
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
	if *plan == nil {
		value := out.AnchorValueSat - int64(r.cfg.FeeSat)
		if value < 330 {
			return nil, bridge.ErrInvalid
		}
		recipient, err := walletOutput(public, d.Request.KeyFamily,
			d.Request.KeyIndex, out.AssetRef, out.Amount, value)
		if err != nil {
			return nil, err
		}
		// Declare the locator so normal tapd wallet operations can
		// spend the recovered asset after proof import.
		_, err = r.cfg.SDK.DeclareScriptKey(ctx,
			&sdk.DeclareScriptKeyRequest{
				ScriptKey: recipient.Script.External.ScriptKey,
			})
		if err != nil {
			return nil, err
		}
		p, err := lastProof(fallbackProof)
		if err != nil {
			return nil, err
		}
		anchor, err := anchorTemplate(
			p, recipient.Anchor.InternalKey, value, spend,
		)
		if err != nil {
			return nil, err
		}
		*plan = customRequest(assetInput(out, fallbackProof), recipient,
			anchor, spend.SigningPlan(0))
		if err := r.save(s); err != nil {
			return nil, err
		}
	}
	if transactionPtr.Package == nil {
		pkg, err := r.commit(ctx, *plan)
		if err != nil {
			return nil, err
		}
		transactionPtr.Package = pkg
		if err := r.save(s); err != nil {
			return nil, err
		}
	}
	if len(transactionPtr.Final) == 0 {
		requests, err := transactionPtr.Package.SigningRequests()
		if err != nil || len(requests.ScriptPath) != 1 {
			return nil, bridge.ErrInvalid
		}
		request := requests.ScriptPath[0]
		signature, err := r.cfg.Signer.ScriptPath(ctx,
			locator(d.Request.KeyFamily, d.Request.KeyIndex),
			transactionPtr.Package, request.ID)
		if err != nil {
			return nil, err
		}
		witness, err := policy.SuccessWitness(out.TaprootAssetRoot,
			signature, preimage)
		if refund {
			witness, err = policy.TimeoutWitness(
				out.TaprootAssetRoot, signature,
			)
		}
		if err != nil {
			return nil, err
		}
		finalized, err := transactionPtr.Package.ApplyScriptPathWitness(
			request.ID, witness[:len(witness)-2])
		if err != nil {
			return nil, err
		}
		transactionPtr.Final = finalized.AnchorPsbt
		if err := r.save(s); err != nil {
			return nil, err
		}
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
	block, _, err := r.findSpend(ctx, point, s.Since)
	if err != nil {
		return nil, err
	}
	found := spendingTransaction(block, point)
	if len(found.TxIn) != 1 {
		return nil, bridge.ErrInvalid
	}
	if len(found.TxOut) != 1 {
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
	witness := found.TxIn[0].Witness
	if len(witness) < 3 ||
		!bytes.Equal(witness[len(witness)-2], spend.LeafScript) ||
		!bytes.Equal(witness[len(witness)-1], spend.ControlBlock) {
		return nil, bridge.ErrPending
	}
	fallbackProof, err := r.publish(ctx, s.Fallback, s.Since)
	if err != nil {
		return nil, err
	}
	p, err := lastProof(fallbackProof)
	if err != nil {
		return nil, err
	}
	recipient, err := walletOutput(public, 0, 0, out.AssetRef,
		out.Amount, found.TxOut[0].Value)
	if err != nil {
		return nil, err
	}
	anchor, err := anchorTemplate(p, recipient.Anchor.InternalKey,
		found.TxOut[0].Value, spend)
	if err != nil {
		return nil, err
	}
	request := customRequest(assetInput(out, fallbackProof),
		recipient, anchor, spend.SigningPlan(0))
	pkg, err := r.commit(ctx, request)
	if err != nil {
		return nil, err
	}
	packet, err := decodePSBT(pkg.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	if packet.UnsignedTx.TxHash() != found.TxHash() {
		return nil, bridge.ErrInvalid
	}
	requests, err := pkg.SigningRequests()
	if err != nil || len(requests.ScriptPath) != 1 {
		return nil, bridge.ErrInvalid
	}
	final, err := pkg.ApplyScriptPathWitness(requests.ScriptPath[0].ID,
		witness[:len(witness)-2])
	if err != nil {
		return nil, err
	}
	raw, err := r.publish(ctx, transaction{Package: pkg,
		Final: final.AnchorPsbt}, s.Since)
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
	p, err := r.verifyProof(ctx, raw, s.Offer.Terms.GroupKey,
		s.Offer.Terms.DepositAmount, requireUnspent)
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
	if sdk.AssetID(p.Asset.ID()) != prior.IssuanceID {
		return bridge.ErrInvalid
	}
	pub := c.ServerKey
	if refund {
		pub = c.ClientKey
	}
	expected, err := walletOutput(
		pub, 0, 0, prior.AssetRef, prior.Amount, 330,
	)
	if err != nil {
		return err
	}
	recipientKey := p.InclusionProof.InternalKey.SerializeCompressed()
	if !bytes.Equal(p.Asset.ScriptKey.PubKey.SerializeCompressed(),
		expected.Script.External.ScriptKey.PubKey[:]) ||
		!bytes.Equal(recipientKey, pub[:]) {
		return bridge.ErrInvalid
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
	block, err := r.cfg.Chain.Block(ctx, p.BlockHeight)
	if err != nil {
		return err
	}
	for _, tx := range block.Transactions {
		if tx.TxHash() != p.AnchorTx.TxHash() {
			continue
		}
		if len(tx.TxIn) != 1 || tx.TxIn[0].PreviousOutPoint !=
			sdkPoint(prior.AnchorOutpoint) {
			return bridge.ErrInvalid
		}
		w := tx.TxIn[0].Witness
		if len(w) < 3 || !bytes.Equal(w[len(w)-2], spend.LeafScript) ||
			!bytes.Equal(w[len(w)-1], spend.ControlBlock) {
			return bridge.ErrInvalid
		}
		prev := wire.NewTxOut(prior.AnchorValueSat, spend.PkScript)
		fetch := txscript.NewCannedPrevOutputFetcher(
			prev.PkScript, prev.Value,
		)
		hashes := txscript.NewTxSigHashes(tx, fetch)
		engine, err := txscript.NewEngine(prev.PkScript, tx, 0,
			txscript.StandardVerifyFlags, nil, hashes,
			prev.Value, fetch)
		if err != nil {
			return err
		}
		return engine.Execute()
	}
	return bridge.ErrInvalid
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
