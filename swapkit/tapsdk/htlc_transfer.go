package tapsdk

import (
	"bytes"
	"reflect"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/tappsbt"
)

const (
	// htlcTxVersion is the transaction version of an HTLC transfer. A
	// zero-fee parent with a pay-to-anchor output is only relayed as a
	// version 3 transaction.
	htlcTxVersion = 3

	// htlcOutputIndex is the fixed position of the HTLC output.
	htlcOutputIndex = 0

	// htlcP2AIndex is the fixed position of the pay-to-anchor output.
	htlcP2AIndex = 1

	// htlcSequence is the input sequence of an HTLC transfer. It leaves
	// BIP68 disabled and the transaction lock time unused.
	htlcSequence = 0xfffffffc
)

// p2aScript is the canonical pay-to-anchor output script. Anyone can spend it
// with an empty witness, so either party can attach a fee-paying child.
var p2aScript = []byte{0x51, 0x02, 0x4e, 0x73}

// FundingContract is the funded contract an HTLC transfer spends: a client
// deposit for Loop In or a server reservation for Loop Out.
type FundingContract interface {
	// AnchorPlan returns the cooperative internal key and refund leaf.
	AnchorPlan() swapkit.AnchorPlan

	// CooperativeParticipants returns the sorted MuSig2 participants.
	CooperativeParticipants() []swapkit.PubKey

	// TimeoutSpend returns the funding sender's unilateral refund path.
	TimeoutSpend(assetRoot swapkit.Hash) (*swapkit.SwapAnchorSpendInfo,
		error)
}

// HTLCTerms contains one party's own view of a cooperative transfer of a
// funded contract output into an HTLC. Every field must come from terms the
// party agreed to or verified itself, never from the peer's package. The same
// terms describe both directions: for Loop In the funding is the client's
// deposit and the server claims the HTLC, for Loop Out the funding is the
// server's reservation and the client claims the HTLC.
type HTLCTerms struct {
	// Funding is the contract of the output being spent.
	Funding FundingContract

	// Input is the funded contract output, as verified from its proof.
	Input sdk.CustomAnchorAssetOutputSummary

	// InputProof is the raw proof file of the funded contract output.
	InputProof []byte

	// HTLC is the contract of the output being created.
	HTLC *swapkit.SwapKit

	// SessionContext is the public MuSig2 session context, for example the
	// payment hash. It must not contain secrets.
	SessionContext []byte
}

// prevout returns the authenticated funding output and its outpoint after
// checking that it carries the funding contract.
func (t HTLCTerms) prevout() (*wire.TxOut, wire.OutPoint, error) {
	if t.Funding == nil || t.HTLC == nil {
		return nil, wire.OutPoint{}, swapkit.ErrInvalid
	}
	p, err := LastProof(t.InputProof)
	if err != nil {
		return nil, wire.OutPoint{}, err
	}
	point := p.OutPoint()
	if point != SDKPoint(t.Input.AnchorOutpoint) ||
		point.Index >= uint32(len(p.AnchorTx.TxOut)) ||
		p.AnchorTx.TxOut[point.Index] == nil {

		return nil, wire.OutPoint{}, swapkit.ErrInvalid
	}
	prevout := p.AnchorTx.TxOut[point.Index]

	// The cooperative signature is only meaningful for an output that is
	// really locked by the funding contract.
	spend, err := t.Funding.TimeoutSpend(t.Input.TaprootAssetRoot)
	if err != nil {
		return nil, wire.OutPoint{}, err
	}
	if !bytes.Equal(spend.PkScript, prevout.PkScript) ||
		spend.TaprootMerkleRoot != t.Input.TaprootMerkleRoot ||
		prevout.Value != t.Input.AnchorValueSat {

		return nil, wire.OutPoint{}, swapkit.ErrInvalid
	}

	return prevout, point, nil
}

// signingPlan returns the cooperative key-path plan of the funding input.
func (t HTLCTerms) signingPlan() sdk.CustomAnchorInputSigningPlan {
	return CooperativeSigningPlan(
		t.Funding.CooperativeParticipants(), 0, t.SessionContext,
	)
}

// HTLCRequest constructs the unsigned SDK request that moves a funded contract
// output into an HTLC. The transaction is a zero-fee version 3 transaction: it
// keeps the whole funding anchor value in the HTLC at output zero and adds a
// zero-value pay-to-anchor output at index one. Its publisher pays the fee with
// a child of that output and submits both as one package, so the fee is chosen
// at broadcast time. maxSweepFee is the fee the HTLC output must still be able
// to pay for its later claim or refund.
func HTLCRequest(terms HTLCTerms, maxSweepFee uint64) (
	*sdk.CustomAnchorRequest, error) {

	if _, _, err := terms.prevout(); err != nil {
		return nil, err
	}
	in := terms.Input
	value := in.AnchorValueSat
	if value < 330 || maxSweepFee > uint64(value-330) {
		return nil, swapkit.ErrInvalid
	}
	output, err := LegacyOPTrueOutput(sdk.CustomAssetOutput{
		ID: "HTLC", AssetRef: in.AssetRef, Amount: in.Amount,
		AnchorOutputIndex: htlcOutputIndex,
		AnchorValueSat:    uint64(value),
	}, terms.HTLC.AnchorPlan())
	if err != nil {
		return nil, err
	}
	p, err := LastProof(terms.InputProof)
	if err != nil {
		return nil, err
	}
	anchor, err := AnchorTemplate(p, output.Anchor.InternalKey, value, nil)
	if err != nil {
		return nil, err
	}
	packet, err := DecodePSBT(anchor)
	if err != nil {
		return nil, err
	}
	internal := terms.Funding.AnchorPlan().InternalKey
	packet.Inputs[0].TaprootInternalKey = internal[1:]
	packet.Inputs[0].TaprootMerkleRoot = in.TaprootMerkleRoot[:]
	packet.UnsignedTx.Version = htlcTxVersion
	packet.UnsignedTx.TxIn[0].Sequence = htlcSequence
	packet.UnsignedTx.AddTxOut(wire.NewTxOut(0, bytes.Clone(p2aScript)))
	packet.Outputs = append(packet.Outputs, psbt.POutput{})
	anchor, err = EncodePSBT(packet)
	if err != nil {
		return nil, err
	}
	input, err := AssetInput(in, terms.InputProof)
	if err != nil {
		return nil, err
	}
	request := CustomRequest(input, output, anchor, terms.signingPlan())
	request.Funding = sdk.CustomAnchorFundingPlan{
		Mode: sdk.CustomAnchorFundingExternalP2AFeeBump,
		ExternalP2AFeeBump: &sdk.CustomAnchorExternalP2AFeeBump{
			P2AOutputIndex: htlcP2AIndex,
		},
	}

	return request, nil
}

// ValidateHTLCPackage binds a peer's native SDK package to the party's own
// HTLC terms before it is co-signed. It accepts only the exact transaction
// that HTLCRequest builds for those terms: the funded output as the single
// input, the whole asset amount and anchor value in the agreed HTLC, a
// zero-value pay-to-anchor output and no fee. It does not check the funding
// proof's history or the chain, and it does not check timelock ordering.
func ValidateHTLCPackage(terms HTLCTerms, raw []byte) (
	*sdk.CustomAnchorTransferPackage, error) {

	prevout, point, err := terms.prevout()
	if err != nil {
		return nil, err
	}
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
	funded := terms.Input
	in, out := pkg.Inputs[0], pkg.Outputs[0]
	if !bytes.Equal(in.ProofSource.Blob, terms.InputProof) ||
		in.AnchorOutpoint != funded.AnchorOutpoint ||
		in.ScriptKey != funded.ScriptKey ||
		in.Amount != funded.Amount ||
		out.Amount != in.Amount || out.IssuanceID != in.IssuanceID ||
		!out.AssetRef.Equivalent(funded.AssetRef) ||
		out.AnchorOutputIndex != htlcOutputIndex ||
		out.AnchorValueSat < 330 {

		return nil, swapkit.ErrInvalid
	}
	// The transfer pays no fee itself. Its pay-to-anchor output lets the
	// publisher choose the fee at broadcast time with a child transaction,
	// instead of fixing it when the swap is signed.
	funding := pkg.Funding
	if funding.Mode != sdk.CustomAnchorFundingExternalP2AFeeBump ||
		funding.P2AOutputIndex == nil ||
		*funding.P2AOutputIndex != htlcP2AIndex {

		return nil, swapkit.ErrInvalid
	}
	// The peer owns the package it hands us, including its publication
	// metadata. A zero-fee parent cannot relay alone, so the publisher
	// submits it together with its child and tapd must only log it. Reject
	// any other publication policy.
	if !pkg.Publish.SkipAnchorTxBroadcast || !pkg.Publish.ExternalBroadcast {
		return nil, swapkit.ErrInvalid
	}
	packet, err := DecodePSBT(pkg.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	tx := packet.UnsignedTx
	if len(tx.TxIn) != 1 || len(tx.TxOut) != 2 ||
		tx.Version != htlcTxVersion || tx.LockTime != 0 ||
		tx.TxIn[0].Sequence != htlcSequence ||
		out.AnchorValueSat != funded.AnchorValueSat ||
		tx.TxOut[htlcP2AIndex].Value != 0 ||
		!bytes.Equal(tx.TxOut[htlcP2AIndex].PkScript, p2aScript) {

		return nil, swapkit.ErrInvalid
	}
	// The MuSig2 sighash is taken over the committed PSBT's own previous
	// output. A peer that only changes the signed input value or script
	// obtains a complete signature that is invalid for the real funding, so
	// bind the signed prevout to the authenticated funding output here.
	committed, err := DecodePSBT(pkg.CommittedAnchorPsbt)
	if err != nil {
		return nil, err
	}
	if len(committed.UnsignedTx.TxIn) != 1 || len(committed.Inputs) != 1 ||
		committed.UnsignedTx.TxIn[0].PreviousOutPoint != point ||
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
	// The output must be exactly the party's own HTLC: its payment hash,
	// expiry and both parties' keys in the agreed roles.
	spend, err := terms.HTLC.TimeoutSpend(out.TaprootAssetRoot)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(spend.PkScript, tx.TxOut[htlcOutputIndex].PkScript) ||
		spend.TaprootMerkleRoot != out.TaprootMerkleRoot {

		return nil, swapkit.ErrInvalid
	}
	if !reflect.DeepEqual(pkg.SigningPlans[0], terms.signingPlan()) {
		return nil, swapkit.ErrInvalid
	}
	if _, err := OPTrueSpendInfo(out); err != nil {
		return nil, err
	}

	return pkg, nil
}
