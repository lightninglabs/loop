package tapsdk

import (
	"bytes"
	"reflect"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
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

// assetInput supplies a known OP_TRUE asset and proof to the SDK builder.
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
		out.AssetRef != c.Output.AssetRef || out.OPTrueSpend == nil ||
		out.AnchorOutputIndex != 0 || out.AnchorValueSat < 330 {

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
	if err := out.OPTrueSpend.Validate(out.ScriptKey); err != nil {
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
	output := OPTrueOutput(sdk.CustomAssetOutput{
		ID: "HTLC", AssetRef: c.Output.AssetRef,
		Amount: c.Output.Amount, AnchorValueSat: uint64(value),
	}, sdk.KeyDescriptor{RawKeyBytes: c.ClientKey,
		KeyLocator: sdk.KeyLocator{Family: uint32(int32(d.Key.Family)),
			Index: d.Key.Index}}, policy.AnchorPlan())
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
	plan := CustomRequest(assetInput(c.Output, d.Deposit.Proof),
		output, anchor, CooperativeSigningPlan(
			dk.CooperativeParticipants(), 0, offer.Hash[:],
		))
	return plan, nil
}
