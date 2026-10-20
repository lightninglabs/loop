package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	kit "github.com/lightninglabs/loop/swapkit/tapsdk"
	"github.com/lightninglabs/loop/swapkit/tapsdk/walletsigner"
	sdk "github.com/lightninglabs/tap-sdk"
	bolt "go.etcd.io/bbolt"
)

type recoveryRequest struct {
	Version     uint32
	Package     *sdk.CustomAnchorTransferPackage
	ClientNonce [66]byte
}

type serverFallback struct {
	Request     recoveryRequest
	ServerNonce [66]byte
	Partial     []byte
}

type swapRecord struct {
	Offer       bridge.Offer
	Since       uint32
	Plan        *sdk.CustomAnchorRequest
	Request     recoveryRequest
	Server      serverFallback
	Fallback    transaction
	ClaimPlan   *sdk.CustomAnchorRequest
	Claim       transaction
	ClaimProof  []byte
	RefundPlan  *sdk.CustomAnchorRequest
	Refund      transaction
	RefundProof []byte
}

func (r *Runtime) swap(offer bridge.Offer) (*swapRecord, error) {
	var record swapRecord
	err := r.get(key("swap/", offer.Hash), &record)
	if missing(err) {
		record.Offer = offer
		return &record, r.put(key("swap/", offer.Hash), record)
	}
	if err != nil {
		return nil, err
	}
	if !equal(record.Offer, offer) {
		return nil, bridge.ErrInvalid
	}
	return &record, nil
}

func (r *Runtime) save(s *swapRecord) error {
	return r.put(key("swap/", s.Offer.Hash), s)
}

func (r *Runtime) localDeposit(point string) (*depositRecord, error) {
	var found *depositRecord
	err := r.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("runtime-v1")).Cursor()
		prefix := []byte("deposit/")
		for k, raw := c.Seek(prefix); k != nil &&
			bytes.HasPrefix(k, prefix); k, raw = c.Next() {
			var record depositRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if record.Deposit.Outpoint == point {
				found = &record
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, bridge.ErrInvalid
	}
	return found, nil
}

func (r *Runtime) offerDeposit(ctx context.Context,
	offer bridge.Offer, unspent bool) (*depositRecord, error) {

	t := offer.Terms
	if t.Version != 1 || len(t.DepositOutpoints) != 1 ||
		t.DepositAmount == 0 || t.ChangeAmount != 0 ||
		t.PayoutAmount > t.DepositAmount ||
		t.AssetFee != t.DepositAmount-t.PayoutAmount {
		return nil, bridge.ErrInvalid
	}
	var record *depositRecord
	if len(t.Admission) > 0 {
		var a bridge.Admission
		if bridge.Decode(t.Admission, &a) != nil ||
			a.Registration.Owner != t.Owner ||
			a.RequestID != t.RequestID ||
			a.GroupKey != t.GroupKey ||
			!bytes.Equal(a.Contract, t.Contract) ||
			len(a.Deposits) != 1 ||
			a.Deposits[0].Outpoint != t.DepositOutpoints[0] ||
			a.RefundHeight != t.RefundHeight {
			return nil, bridge.ErrInvalid
		}
		err := r.checkKey(ctx, a.Registration.KeyFamily,
			a.Registration.KeyIndex, a.Registration.ServerKey)
		if err != nil {
			return nil, err
		}
		var c depositContract
		if bridge.Decode(t.Contract, &c) != nil ||
			c.ClientKey != a.Registration.ClientKey ||
			c.ServerKey != a.Registration.ServerKey ||
			c.CSV != a.Registration.CSV {
			return nil, bridge.ErrInvalid
		}
		d := a.Deposits[0]
		d.Contract = t.Contract
		d.Amount, d.GroupKey = t.DepositAmount, t.GroupKey
		record = &depositRecord{Deposit: d,
			Request: bridge.CreateDepositRequest{
				ClientKey: c.ClientKey, ServerKey: c.ServerKey,
				CSV: c.CSV, KeyFamily: a.Registration.KeyFamily,
				KeyIndex: a.Registration.KeyIndex,
			}}
	} else {
		var err error
		record, err = r.localDeposit(t.DepositOutpoints[0])
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(record.Deposit.Contract, t.Contract) ||
			record.Deposit.GroupKey != t.GroupKey ||
			record.Deposit.Amount != t.DepositAmount {
			return nil, bridge.ErrInvalid
		}
	}
	p, err := r.verifyDeposit(ctx, record.Deposit, unspent)
	if err != nil {
		return nil, err
	}
	if uint64(p.BlockHeight)+uint64(record.Request.CSV) <=
		uint64(t.RefundHeight)+uint64(t.RecoveryDelta) {
		return nil, bridge.ErrInvalid
	}
	record.Since = p.BlockHeight
	return record, nil
}

func htlc(offer bridge.Offer, c depositContract) (*kit.SwapKit, error) {
	return kit.NewSwapKit(kit.SwapKitParams{
		Policy:    kit.SwapHTLCStaticLoopInV3,
		SenderKey: c.ClientKey, ReceiverKey: c.ServerKey,
		SenderInternalKey:   c.ClientKey,
		ReceiverInternalKey: c.ServerKey,
		PaymentHash:         offer.Hash,
		Expiry:              offer.Terms.RefundHeight,
	})
}

func roundID(hash [32]byte) [32]byte {
	return sha256.Sum256(append([]byte("loop/fallback/v1/"), hash[:]...))
}

func (r *Runtime) prepareRecovery(ctx context.Context,
	offer bridge.Offer) ([]byte, error) {

	d, err := r.offerDeposit(ctx, offer, true)
	if err != nil {
		return nil, err
	}
	s, err := r.swap(offer)
	if err != nil {
		return nil, err
	}
	if s.Since == 0 {
		s.Since, err = r.cfg.Chain.Height(ctx)
		if err != nil {
			return nil, err
		}
		if err := r.save(s); err != nil {
			return nil, err
		}
	}
	var c depositContract
	if err := json.Unmarshal(d.Deposit.Contract, &c); err != nil {
		return nil, err
	}
	if s.Plan == nil {
		policy, err := htlc(offer, c)
		if err != nil {
			return nil, err
		}
		keys, err := r.cfg.Wallet.DeriveKeys(ctx)
		if err != nil {
			return nil, err
		}
		value := c.Output.AnchorValueSat - int64(r.cfg.FeeSat)
		if value < 330+int64(r.cfg.MaxFeeSat) {
			return nil, bridge.ErrInvalid
		}
		output, err := policy.Output(kit.SwapOutputParams{
			ID: "htlc", AssetRef: c.Output.AssetRef,
			Amount:           c.Output.Amount,
			AnchorValueSat:   uint64(value),
			AssetInternalKey: keys.ScriptKey.KeyDesc,
		})
		if err != nil {
			return nil, err
		}
		p, err := lastProof(d.Deposit.Proof)
		if err != nil {
			return nil, err
		}
		anchor, err := anchorTemplate(
			p, output.Anchor.InternalKey, value, nil,
		)
		if err != nil {
			return nil, err
		}
		packet, err := decodePSBT(anchor)
		if err != nil {
			return nil, err
		}
		dk, err := kit.NewDepositKit(kit.DepositKitParams{
			ClientKey: c.ClientKey, ServerKey: c.ServerKey,
			CSVExpiry: c.CSV,
		})
		if err != nil {
			return nil, err
		}
		internal := dk.AnchorPlan().InternalKey.PubKey
		packet.Inputs[0].TaprootInternalKey = internal[1:]
		merkle := c.Output.TaprootMerkleRoot[:]
		packet.Inputs[0].TaprootMerkleRoot = merkle
		anchor, err = encodePSBT(packet)
		if err != nil {
			return nil, err
		}
		signing := dk.CooperativeSigningPlan(0, offer.Hash[:])
		s.Plan = customRequest(assetInput(c.Output, d.Deposit.Proof),
			output, anchor, signing)
		if err := r.save(s); err != nil {
			return nil, err
		}
	}
	if s.Request.Package == nil {
		pkg, err := r.commit(ctx, s.Plan)
		if err != nil {
			return nil, err
		}
		s.Request = recoveryRequest{Version: 1, Package: pkg}
		if err := r.save(s); err != nil {
			return nil, err
		}
	}
	requests, err := s.Request.Package.SigningRequests()
	if err != nil || len(requests.MuSig2) != 1 {
		return nil, bridge.ErrInvalid
	}
	nonce, err := r.cfg.Signer.Nonce(ctx, roundID(offer.Hash),
		locator(d.Request.KeyFamily, d.Request.KeyIndex),
		s.Request.Package, requests.MuSig2[0].ID)
	if err != nil {
		return nil, err
	}
	s.Request.ClientNonce = nonce
	if err := r.save(s); err != nil {
		return nil, err
	}
	return json.Marshal(s.Request)
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

func (r *Runtime) validateRecovery(ctx context.Context, offer bridge.Offer,
	raw []byte) (*recoveryRequest, error) {

	d, err := r.offerDeposit(ctx, offer, true)
	if err != nil {
		return nil, err
	}
	var request recoveryRequest
	if bridge.Decode(raw, &request) != nil || request.Version != 1 ||
		request.Package == nil || request.ClientNonce == ([66]byte{}) {
		return nil, bridge.ErrInvalid
	}
	pkg := request.Package
	if err := pkg.Validate(); err != nil {
		return nil, err
	}
	requests, err := pkg.SigningRequests()
	if err != nil || len(requests.MuSig2) != 1 ||
		requests.MuSig2[0].SighashType != 0 {
		return nil, bridge.ErrInvalid
	}
	if len(pkg.Inputs) != 1 || len(pkg.Outputs) != 1 ||
		len(pkg.PassiveVirtualPsbts) != 0 ||
		len(pkg.SigningPlans) != 1 ||
		len(pkg.BackendManagedInputIndices) != 0 {
		return nil, bridge.ErrInvalid
	}
	var c depositContract
	if err := json.Unmarshal(d.Deposit.Contract, &c); err != nil {
		return nil, err
	}
	in, out := pkg.Inputs[0], pkg.Outputs[0]
	if !bytes.Equal(in.ProofSource.Blob, d.Deposit.Proof) ||
		in.AnchorOutpoint != c.Output.AnchorOutpoint ||
		in.ScriptKey != c.Output.ScriptKey ||
		in.Amount != c.Output.Amount ||
		out.Amount != in.Amount || out.IssuanceID != in.IssuanceID ||
		out.AssetRef != c.Output.AssetRef || out.OPTrueSpend == nil ||
		out.AnchorOutputIndex != 0 || out.AnchorValueSat < 330 {
		return nil, bridge.ErrInvalid
	}
	packet, err := decodePSBT(pkg.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	tx := packet.UnsignedTx
	fee := c.Output.AnchorValueSat - out.AnchorValueSat
	if len(tx.TxIn) != 1 || len(tx.TxOut) != 1 || tx.Version != 2 ||
		tx.LockTime != 0 || tx.TxIn[0].Sequence != 0xfffffffc ||
		fee <= 0 || uint64(fee) > r.cfg.MaxFeeSat ||
		out.AnchorValueSat < 330+int64(r.cfg.FeeSat) {
		return nil, bridge.ErrInvalid
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
		return nil, bridge.ErrInvalid
	}
	dk, err := kit.NewDepositKit(kit.DepositKitParams{
		ClientKey: c.ClientKey, ServerKey: c.ServerKey,
		CSVExpiry: c.CSV,
	})
	if err != nil {
		return nil, err
	}
	signing := dk.CooperativeSigningPlan(0, offer.Hash[:])
	if !equal(pkg.SigningPlans[0], signing) {
		return nil, bridge.ErrInvalid
	}
	if err := out.OPTrueSpend.Validate(out.ScriptKey); err != nil {
		return nil, err
	}
	return &request, nil
}

func (r *Runtime) buildFallback(ctx context.Context,
	op bridge.Operation) (bridge.Bundle, error) {

	req, err := r.validateRecovery(ctx, op.Offer,
		op.Preparation.RecoveryRequest)
	if err != nil {
		return bridge.Bundle{}, err
	}
	s, err := r.swap(op.Offer)
	if err != nil {
		return bridge.Bundle{}, err
	}
	if s.Since == 0 {
		s.Since, err = r.cfg.Chain.Height(ctx)
		if err != nil {
			return bridge.Bundle{}, err
		}
		if err := r.save(s); err != nil {
			return bridge.Bundle{}, err
		}
	}
	if s.Request.Package != nil && !equal(s.Request, *req) {
		return bridge.Bundle{}, bridge.ErrInvalid
	}
	s.Request = *req
	if err := r.save(s); err != nil {
		return bridge.Bundle{}, err
	}
	d, err := r.offerDeposit(ctx, op.Offer, true)
	if err != nil {
		return bridge.Bundle{}, err
	}
	requests, err := req.Package.SigningRequests()
	if err != nil || len(requests.MuSig2) != 1 {
		return bridge.Bundle{}, bridge.ErrInvalid
	}
	id := roundID(op.Offer.Hash)
	nonce, err := r.cfg.Signer.Nonce(ctx, id,
		locator(d.Request.KeyFamily, d.Request.KeyIndex),
		req.Package, requests.MuSig2[0].ID)
	if err != nil {
		return bridge.Bundle{}, err
	}
	partial, err := r.cfg.Signer.Sign(ctx, id, []walletsigner.PeerNonce{{
		PublicKey: d.Request.ClientKey, Nonce: req.ClientNonce,
	}})
	if err != nil {
		return bridge.Bundle{}, err
	}
	s.Server = serverFallback{Request: *req, ServerNonce: nonce,
		Partial: partial.Signature}
	if err := r.save(s); err != nil {
		return bridge.Bundle{}, err
	}
	raw, err := json.Marshal(s.Server)
	return bridge.Bundle{Data: raw}, err
}

func (r *Runtime) finalizeFallback(ctx context.Context,
	op bridge.Operation) (bridge.Fallback, error) {

	var bundle serverFallback
	if bridge.Decode(op.Bundle.Data, &bundle) != nil {
		return bridge.Fallback{}, bridge.ErrInvalid
	}
	s, err := r.swap(op.Offer)
	if err != nil {
		return bridge.Fallback{}, err
	}
	if !equal(s.Request, bundle.Request) {
		return bridge.Fallback{}, bridge.ErrInvalid
	}
	d, err := r.offerDeposit(ctx, op.Offer, true)
	if err != nil {
		return bridge.Fallback{}, err
	}
	id := roundID(op.Offer.Hash)
	_, err = r.cfg.Signer.Sign(ctx, id, []walletsigner.PeerNonce{{
		PublicKey: d.Request.ServerKey, Nonce: bundle.ServerNonce,
	}})
	if err != nil {
		return bridge.Fallback{}, err
	}
	peers := []walletsigner.PeerPartial{{
		PublicKey: d.Request.ServerKey, Signature: bundle.Partial,
	}}
	signature, err := r.cfg.Signer.Combine(ctx, id, peers)
	if err != nil {
		return bridge.Fallback{}, err
	}
	requests, err := s.Request.Package.SigningRequests()
	if err != nil {
		return bridge.Fallback{}, err
	}
	pkg, err := s.Request.Package.ApplyKeyPathSignature(
		requests.MuSig2[0].ID, signature)
	if err != nil {
		return bridge.Fallback{}, err
	}
	packet, err := decodePSBT(pkg.AnchorPsbt)
	if err != nil {
		return bridge.Fallback{}, err
	}
	if err := psbt.MaybeFinalizeAll(packet); err != nil {
		return bridge.Fallback{}, err
	}
	final, err := encodePSBT(packet)
	if err != nil {
		return bridge.Fallback{}, err
	}
	s.Fallback = transaction{Package: s.Request.Package, Final: final}
	if _, err := finalTx(s.Fallback); err != nil {
		return bridge.Fallback{}, err
	}
	if err := r.save(s); err != nil {
		return bridge.Fallback{}, err
	}
	raw, err := json.Marshal(s.Fallback)
	return bridge.Fallback{Bundle: raw, ClaimSignatures: raw}, err
}

func (r *Runtime) verifyClaim(ctx context.Context,
	op bridge.Operation) (bridge.Claim, error) {

	s, err := r.swap(op.Offer)
	if err != nil {
		return bridge.Claim{}, err
	}
	var bundle serverFallback
	if bridge.Decode(op.Bundle.Data, &bundle) != nil ||
		!equal(bundle, s.Server) {
		return bridge.Claim{}, bridge.ErrInvalid
	}
	var proposed transaction
	if bridge.Decode(op.Signatures, &proposed) != nil ||
		proposed.Package == nil {
		return bridge.Claim{}, bridge.ErrInvalid
	}
	s.Fallback = transaction{
		Package: s.Request.Package, Final: proposed.Final,
	}
	if _, err := finalTx(s.Fallback); err != nil {
		return bridge.Claim{}, err
	}
	if err := r.save(s); err != nil {
		return bridge.Claim{}, err
	}
	raw, err := json.Marshal(s.Fallback)
	return bridge.Claim{Data: raw}, err
}
