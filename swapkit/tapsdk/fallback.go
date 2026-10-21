package tapsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

//nolint:tagliatelle // Preserve existing serialized round field names.
type serverFallback struct {
	PackageDigest sdk.Hash `json:"PackageDigest"`
	ClientNonce   [66]byte `json:"ClientNonce"`
	ServerNonce   [66]byte `json:"ServerNonce"`
	Partial       []byte   `json:"Partial"`
}

// swapContext is reconstructed from the normal swap record on every action.
// The unsigned package and completed fallback have exactly one durable owner.
type swapContext struct {
	Offer    swapkit.Offer
	Deposit  *depositRecord
	Fallback *sdk.CustomAnchorTransferPackage
}

func (r *Driver) swap(ctx context.Context,
	op swapkit.Operation) (*swapContext, error) {

	s, err := r.preparedSwap(ctx, op.Offer,
		op.Preparation.RecoveryRequest, false)
	if err != nil {
		return nil, err
	}
	raw := op.Fallback.Bundle
	if len(op.Claim.Data) > 0 {
		raw = op.Claim.Data
	}
	if len(raw) > 0 {
		s.Fallback, err = bindFinalPackage(s.Fallback, raw)
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

// preparedSwap verifies the admitted deposit once for this operation. All
// lower helpers use this context; no validation survives the operation.
func (r *Driver) preparedSwap(ctx context.Context, offer swapkit.Offer,
	raw []byte, unspent bool) (*swapContext, error) {

	d, err := r.offerDeposit(ctx, offer, unspent)
	if err != nil {
		return nil, err
	}
	s := &swapContext{Offer: offer, Deposit: d}
	if len(raw) > 0 {
		s.Fallback, err = validateRecoveryPackage(offer, d, raw)
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (r *Driver) localDeposit(ctx context.Context, point string) (
	*depositRecord, error) {

	d, key, err := r.cfg.Deposit(ctx, point)
	if err != nil {
		return nil, err
	}
	var c depositContract
	if err := swapkit.Decode(d.Contract, &c); err != nil {
		return nil, err
	}
	return &depositRecord{Deposit: d, Contract: c,
		Request: swapkit.CreateDepositRequest{
			ClientKey: c.ClientKey, ServerKey: c.ServerKey, CSV: c.CSV,
			KeyFamily: int32(key.Family), KeyIndex: key.Index,
		}}, nil
}

func (r *Driver) offerDeposit(ctx context.Context,
	offer swapkit.Offer, unspent bool) (*depositRecord, error) {

	t := offer.Terms
	if t.Version != 1 || len(t.DepositOutpoints) != 1 ||
		t.DepositAmount == 0 || t.ChangeAmount != 0 ||
		t.PayoutAmount > t.DepositAmount ||
		t.AssetFee != t.DepositAmount-t.PayoutAmount {

		return nil, swapkit.ErrInvalid
	}
	var record *depositRecord
	if len(t.Admission) > 0 {
		var a swapkit.Admission
		if swapkit.Decode(t.Admission, &a) != nil ||
			a.Registration.Owner != t.Owner ||
			a.RequestID != t.RequestID ||
			a.GroupKey != t.GroupKey ||
			!bytes.Equal(a.Contract, t.Contract) ||
			len(a.Deposits) != 1 ||
			a.Deposits[0].Outpoint != t.DepositOutpoints[0] ||
			a.RefundHeight != t.RefundHeight {

			return nil, swapkit.ErrInvalid
		}
		err := r.checkKey(ctx, a.Registration.KeyFamily,
			a.Registration.KeyIndex, a.Registration.ServerKey)
		if err != nil {
			return nil, err
		}
		var c depositContract
		if swapkit.Decode(t.Contract, &c) != nil ||
			c.ClientKey != a.Registration.ClientKey ||
			c.ServerKey != a.Registration.ServerKey ||
			c.CSV != a.Registration.CSV {

			return nil, swapkit.ErrInvalid
		}
		d := a.Deposits[0]
		d.Contract = t.Contract
		d.Amount, d.GroupKey = t.DepositAmount, t.GroupKey
		record = &depositRecord{Deposit: d, Contract: c,
			Request: swapkit.CreateDepositRequest{
				ClientKey: c.ClientKey, ServerKey: c.ServerKey,
				CSV: c.CSV, KeyFamily: a.Registration.KeyFamily,
				KeyIndex: a.Registration.KeyIndex,
			}}
	} else {
		var err error
		record, err = r.localDeposit(ctx, t.DepositOutpoints[0])
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(record.Deposit.Contract, t.Contract) ||
			record.Deposit.GroupKey != t.GroupKey ||
			record.Deposit.Amount != t.DepositAmount {

			return nil, swapkit.ErrInvalid
		}
	}
	p, err := r.verifyDepositContract(
		ctx, record.Deposit, record.Contract, unspent,
	)
	if err != nil {
		return nil, err
	}
	if uint64(p.BlockHeight)+uint64(record.Request.CSV) <=
		uint64(t.RefundHeight)+uint64(t.RecoveryDelta) {

		return nil, swapkit.ErrInvalid
	}
	record.Proof = p
	return record, nil
}

func htlc(offer swapkit.Offer, c depositContract) (*swapkit.SwapKit, error) {
	return swapkit.NewSwapKit(swapkit.SwapKitParams{
		Policy:    swapkit.SwapHTLCStaticLoopInV3,
		SenderKey: c.ClientKey, ReceiverKey: c.ServerKey,
		SenderInternalKey:   c.ClientKey,
		ReceiverInternalKey: c.ServerKey,
		PaymentHash:         offer.Hash,
		Expiry:              offer.Terms.RefundHeight,
	})
}

// PrepareRecovery builds only the immutable unsigned fallback. Its output is
// saved with the normal Loop In request before payment or signing starts.
func (r *Driver) PrepareRecovery(ctx context.Context,
	offer swapkit.Offer) ([]byte, error) {

	d, err := r.offerDeposit(ctx, offer, true)
	if err != nil {
		return nil, err
	}
	c := d.Contract
	policy, err := htlc(offer, c)
	if err != nil {
		return nil, err
	}
	value := c.Output.AnchorValueSat - int64(r.cfg.FeeSat)
	if value < 330+int64(r.cfg.MaxFeeSat) {
		return nil, swapkit.ErrInvalid
	}
	output := OPTrueOutput(sdk.CustomAssetOutput{
		ID: "htlc", AssetRef: c.Output.AssetRef,
		Amount: c.Output.Amount, AnchorValueSat: uint64(value),
	}, sdk.KeyDescriptor{RawKeyBytes: c.ClientKey,
		KeyLocator: sdk.KeyLocator{Family: uint32(d.Request.KeyFamily),
			Index: d.Request.KeyIndex}}, policy.AnchorPlan())
	p := d.Proof
	anchor, err := anchorTemplate(p, output.Anchor.InternalKey, value, nil)
	if err != nil {
		return nil, err
	}
	packet, err := decodePSBT(anchor)
	if err != nil {
		return nil, err
	}
	dk, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: c.ClientKey, ServerKey: c.ServerKey, CSVExpiry: c.CSV,
	})
	if err != nil {
		return nil, err
	}
	internal := dk.AnchorPlan().InternalKey
	packet.Inputs[0].TaprootInternalKey = internal[1:]
	packet.Inputs[0].TaprootMerkleRoot = c.Output.TaprootMerkleRoot[:]
	anchor, err = encodePSBT(packet)
	if err != nil {
		return nil, err
	}
	plan := customRequest(assetInput(c.Output, d.Deposit.Proof),
		output, anchor, CooperativeSigningPlan(
			dk.CooperativeParticipants(), 0, offer.Hash[:],
		))
	pkg, err := r.commit(ctx, plan)
	if err != nil {
		return nil, err
	}
	return pkg.MarshalBinary()
}

func assetInput(out sdk.CustomAnchorAssetOutputSummary,
	raw []byte) sdk.CustomAssetInput {

	return sdk.CustomAssetInput{
		ID: out.LogicalOutputID, AssetRef: out.AssetRef,
		Amount:    out.Amount,
		ProofFile: raw, Witness: sdk.CustomAssetWitnessPlan{
			Mode:  sdk.CustomAssetWitnessCallerProvided,
			Stack: out.OPTrueSpend.WitnessStack(),
		},
	}
}

func validateRecoveryPackage(offer swapkit.Offer, d *depositRecord,
	raw []byte) (*sdk.CustomAnchorTransferPackage, error) {

	pkg, err := decodePackage(raw)
	if err != nil {
		return nil, err
	}
	requests, err := pkg.SigningRequests()
	if err != nil || len(requests.MuSig2) != 1 ||
		requests.MuSig2[0].SighashType != 0 {

		return nil, swapkit.ErrInvalid
	}
	if len(pkg.Inputs) != 1 || len(pkg.Outputs) != 1 ||
		len(pkg.PassiveVirtualPsbts) != 0 ||
		len(pkg.SigningPlans) != 1 ||
		len(pkg.BackendManagedInputIndices) != 0 {

		return nil, swapkit.ErrInvalid
	}
	c := d.Contract
	in, out := pkg.Inputs[0], pkg.Outputs[0]
	if !bytes.Equal(in.ProofSource.Blob, d.Deposit.Proof) ||
		in.AnchorOutpoint != c.Output.AnchorOutpoint ||
		in.ScriptKey != c.Output.ScriptKey ||
		in.Amount != c.Output.Amount ||
		out.Amount != in.Amount || out.IssuanceID != in.IssuanceID ||
		out.AssetRef != c.Output.AssetRef || out.OPTrueSpend == nil ||
		out.AnchorOutputIndex != 0 || out.AnchorValueSat < 330 {

		return nil, swapkit.ErrInvalid
	}
	packet, err := decodePSBT(pkg.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	tx := packet.UnsignedTx
	fee := c.Output.AnchorValueSat - out.AnchorValueSat
	if len(tx.TxIn) != 1 || len(tx.TxOut) != 1 || tx.Version != 2 ||
		tx.LockTime != 0 || tx.TxIn[0].Sequence != 0xfffffffc ||
		fee <= 0 || out.AnchorValueSat-330 < fee {

		return nil, swapkit.ErrInvalid
	}
	policy, err := htlc(offer, c)
	if err != nil {
		return nil, err
	}
	spend, err := policy.TimeoutSpend(out.TaprootAssetRoot)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(spend.PkScript, tx.TxOut[0].PkScript) ||
		spend.TaprootMerkleRoot != out.TaprootMerkleRoot {

		return nil, swapkit.ErrInvalid
	}
	dk, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: c.ClientKey, ServerKey: c.ServerKey,
		CSVExpiry: c.CSV,
	})
	if err != nil {
		return nil, err
	}
	signing := CooperativeSigningPlan(
		dk.CooperativeParticipants(), 0, offer.Hash[:],
	)
	if !reflect.DeepEqual(pkg.SigningPlans[0], signing) {
		return nil, swapkit.ErrInvalid
	}
	if err := out.OPTrueSpend.Validate(out.ScriptKey); err != nil {
		return nil, err
	}
	return pkg, nil
}

func (r *Driver) BuildFallback(ctx context.Context,
	op swapkit.Operation) (swapkit.Bundle, error) {

	s, err := r.preparedSwap(ctx, op.Offer,
		op.Preparation.RecoveryRequest, true)
	if err != nil {
		return swapkit.Bundle{}, err
	}
	if s.Fallback == nil || op.Nonce == ([66]byte{}) {
		return swapkit.Bundle{}, swapkit.ErrInvalid
	}
	d := s.Deposit
	round, err := NewSigningRound(ctx, r.cfg.Signer,
		locator(d.Request.KeyFamily, d.Request.KeyIndex), s.Fallback)
	if err != nil {
		return swapkit.Bundle{}, err
	}
	defer round.Close(ctx)
	partial, err := round.Partial(ctx, op.Nonce)
	if err != nil {
		return swapkit.Bundle{}, err
	}
	raw, err := json.Marshal(serverFallback{
		PackageDigest: s.Fallback.CommittedPackageDigest,
		ClientNonce:   op.Nonce, ServerNonce: round.Nonce(),
		Partial: partial,
	})
	return swapkit.Bundle{Data: raw}, err
}

// BeginFallback owns no persistent secret. Its caller closes the round after
// the complete fallback is saved, or abandons it after any ambiguous failure.
func (r *Driver) BeginFallback(ctx context.Context, offer swapkit.Offer,
	raw []byte) (*swapkit.FallbackRound, error) {

	s, err := r.preparedSwap(ctx, offer, raw, true)
	if err != nil {
		return nil, err
	}
	if s.Fallback == nil {
		return nil, swapkit.ErrInvalid
	}
	d := s.Deposit
	round, err := NewSigningRound(ctx, r.cfg.Signer,
		locator(d.Request.KeyFamily, d.Request.KeyIndex), s.Fallback)
	if err != nil {
		return nil, err
	}
	return &swapkit.FallbackRound{
		Nonce: round.Nonce(), Close: func() { round.Close(ctx) },
		Finalize: func(ctx context.Context, raw []byte) (
			swapkit.Fallback, error) {

			var b serverFallback
			if swapkit.Decode(raw, &b) != nil ||
				b.ClientNonce != round.Nonce() ||
				b.PackageDigest != s.Fallback.CommittedPackageDigest {

				return swapkit.Fallback{}, swapkit.ErrInvalid
			}
			signed, err := round.Finalize(ctx, b.ServerNonce, b.Partial)
			if err != nil {
				return swapkit.Fallback{}, err
			}
			packet, err := decodePSBT(signed.AnchorPsbt)
			if err != nil {
				return swapkit.Fallback{}, err
			}
			if err := psbt.MaybeFinalizeAll(packet); err != nil {
				return swapkit.Fallback{}, err
			}
			final, err := encodePSBT(packet)
			if err != nil {
				return swapkit.Fallback{}, err
			}
			completed, err := s.Fallback.WithFinalAnchorPSBT(final)
			if err != nil {
				return swapkit.Fallback{}, err
			}
			result, err := completed.MarshalBinary()
			return swapkit.Fallback{
				Bundle: result, ClaimSignatures: result,
			}, err
		},
	}, nil
}

func (r *Driver) VerifyClaim(ctx context.Context,
	op swapkit.Operation) (swapkit.Claim, error) {

	s, err := r.preparedSwap(ctx, op.Offer,
		op.Preparation.RecoveryRequest, false)
	if err != nil {
		return swapkit.Claim{}, err
	}
	// A delayed final from an earlier nonce round is still valid for the
	// same committed package. The SDK verifies its complete witness.
	completed, err := bindFinalPackage(s.Fallback, op.Signatures)
	if err != nil {
		return swapkit.Claim{}, err
	}
	raw, err := completed.MarshalBinary()
	return swapkit.Claim{Data: raw}, err
}
