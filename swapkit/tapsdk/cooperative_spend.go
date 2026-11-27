package tapsdk

import (
	"bytes"
	"reflect"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

// CooperativeSpendTerms contains the funder's own view of a cooperative spend
// of a funded contract output: a client deposit for Loop In or a server
// reservation for Loop Out. Every field must come from terms the funder agreed
// to or verified itself, never from the peer's package.
type CooperativeSpendTerms struct {
	// Funding is the contract of the output being spent.
	Funding FundingContract

	// Input is the funded contract output, as verified from its proof.
	Input sdk.CustomAnchorAssetOutputSummary

	// InputProof is the raw proof file of the funded contract output.
	InputProof []byte

	// SessionContext is the public MuSig2 session context, for example the
	// payment hash. It must not contain secrets.
	SessionContext []byte
}

// ValidateCooperativeSpend is the funder's check of a peer's package that
// spends a funded contract output cooperatively, before the funder co-signs
// it. A swap ends this way once it is settled in the funder's favor off chain,
// so the funder does not care where the asset goes and the outputs are not
// constrained. Its signature must still not be usable for anything else.
//
// The package is accepted only if it spends the funded output as its single
// input and nothing besides: no further asset, passive or wallet input. The
// transaction that is signed must commit to the authenticated funding output's
// own value and script, and the signing plan must be the funding contract's
// cooperative key path under the agreed session context.
//
// It does not check the funding proof's history or the chain, and it does not
// decide whether the swap is settled. Releasing the signature before the
// funder is paid, or holds the preimage, hands the funding to the peer.
func ValidateCooperativeSpend(terms CooperativeSpendTerms, raw []byte) (
	*sdk.CustomAnchorTransferPackage, error) {

	prevout, point, err := fundedPrevout(
		terms.Funding, terms.Input, terms.InputProof,
	)
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
	if len(pkg.Inputs) != 1 || len(pkg.PassiveVirtualPsbts) != 0 ||
		len(pkg.SigningPlans) != 1 ||
		len(pkg.BackendManagedInputIndices) != 0 {

		return nil, swapkit.ErrInvalid
	}
	funded := terms.Input
	in := pkg.Inputs[0]
	if !bytes.Equal(in.ProofSource.Blob, terms.InputProof) ||
		in.AnchorOutpoint != funded.AnchorOutpoint ||
		in.ScriptKey != funded.ScriptKey ||
		in.Amount != funded.Amount {

		return nil, swapkit.ErrInvalid
	}

	// The MuSig2 sighash is taken over the committed PSBT's own previous
	// output, and it commits to every input of the transaction. Bind the
	// single signed input to the authenticated funding output.
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
	signing := CooperativeSigningPlan(
		terms.Funding.CooperativeParticipants(), 0, terms.SessionContext,
	)
	if !reflect.DeepEqual(pkg.SigningPlans[0], signing) {
		return nil, swapkit.ErrInvalid
	}

	return pkg, nil
}
