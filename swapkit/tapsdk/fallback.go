package tapsdk

import (
	"bytes"
	"reflect"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/tappsbt"
)

// HTLC constructs the existing V3 Bitcoin contract for the offered swap.
func HTLC(offer swapkit.Offer, c DepositContract) (*swapkit.SwapKit, error) {
	return swapkit.NewSwapKit(swapkit.SwapKitParams{
		Policy:    swapkit.SwapHTLCStaticLoopInV3,
		SenderKey: c.ClientKey, ReceiverKey: c.ServerKey,
		SenderInternalKey:   c.ClientKey,
		ReceiverInternalKey: c.ServerKey,
		PaymentHash:         offer.Hash,
		Expiry:              offer.Terms.RefundHeight,
	})
}

// validateRecoveryPackage binds a native SDK transfer to the offered fallback.
func validateRecoveryPackage(offer swapkit.Offer, d *DepositContext,
	raw []byte) (*sdk.CustomAnchorTransferPackage, error) {

	pkg, err := DecodePackage(raw)
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
		!out.AssetRef.Equivalent(c.Output.AssetRef) ||
		out.AnchorOutputIndex != 0 || out.AnchorValueSat < 330 {

		return nil, swapkit.ErrInvalid
	}
	// The peer owns the package it hands us, including its publication
	// metadata. A fallback must be broadcast normally, so reject a package
	// that would be logged without reaching the network.
	if pkg.Publish.SkipAnchorTxBroadcast || pkg.Publish.ExternalBroadcast {
		return nil, swapkit.ErrInvalid
	}
	packet, err := DecodePSBT(pkg.AnchorPsbt)
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
	// The MuSig2 sighash is taken over the committed PSBT's own previous
	// output. A peer that only changes the signed input value or script
	// obtains a complete signature that is invalid for the real deposit, so
	// bind the signed prevout to the authenticated deposit output here.
	committed, err := DecodePSBT(pkg.CommittedAnchorPsbt)
	if err != nil {
		return nil, err
	}
	prevout := d.Proof.AnchorTx.TxOut[d.Proof.OutPoint().Index]
	if len(committed.UnsignedTx.TxIn) != 1 || len(committed.Inputs) != 1 ||
		committed.UnsignedTx.TxIn[0].PreviousOutPoint !=
			d.Proof.OutPoint() ||
		committed.Inputs[0].WitnessUtxo == nil ||
		committed.Inputs[0].WitnessUtxo.Value != prevout.Value ||
		!bytes.Equal(committed.Inputs[0].WitnessUtxo.PkScript,
			prevout.PkScript) {

		return nil, swapkit.ErrInvalid
	}
	// The Bitcoin transaction can be valid while the committed asset carries
	// its own timelock that tapd's VM rejects at every height below it. The
	// kit never agreed to such an asset, so reject it before signing.
	if len(pkg.ActiveVirtualPsbts) != 1 {
		return nil, swapkit.ErrInvalid
	}
	vPkt, err := tappsbt.Decode(pkg.ActiveVirtualPsbts[0])
	if err != nil {
		return nil, err
	}
	if len(vPkt.Outputs) != 1 {
		return nil, swapkit.ErrInvalid
	}
	vOut := vPkt.Outputs[0]
	if vOut.LockTime != 0 || vOut.RelativeLockTime != 0 ||
		vOut.Asset == nil || vOut.Asset.LockTime != 0 ||
		vOut.Asset.RelativeLockTime != 0 {

		return nil, swapkit.ErrInvalid
	}
	policy, err := HTLC(offer, c)
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
	if _, err := OPTrueSpendInfo(out); err != nil {
		return nil, err
	}
	return pkg, nil
}

// FallbackRequest constructs the unsigned SDK request for one validated offer.
func FallbackRequest(s *SwapContext, fee, maxFee uint64) (
	*sdk.CustomAnchorRequest, error) {

	d, offer := s.Deposit, s.Offer
	c := d.Contract
	policy, err := HTLC(offer, c)
	if err != nil {
		return nil, err
	}
	value := c.Output.AnchorValueSat - int64(fee)
	if value < 330+int64(maxFee) {
		return nil, swapkit.ErrInvalid
	}
	output, err := LegacyOPTrueOutput(sdk.CustomAssetOutput{
		ID: "HTLC", AssetRef: c.Output.AssetRef,
		Amount: c.Output.Amount, AnchorValueSat: uint64(value),
	}, policy.AnchorPlan())
	if err != nil {
		return nil, err
	}
	p := d.Proof
	anchor, err := AnchorTemplate(p, output.Anchor.InternalKey, value, nil)
	if err != nil {
		return nil, err
	}
	packet, err := DecodePSBT(anchor)
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
	anchor, err = EncodePSBT(packet)
	if err != nil {
		return nil, err
	}
	input, err := AssetInput(c.Output, d.Deposit.Proof)
	if err != nil {
		return nil, err
	}
	plan := CustomRequest(input,
		output, anchor, CooperativeSigningPlan(
			dk.CooperativeParticipants(), 0, offer.Hash[:],
		))
	return plan, nil
}
