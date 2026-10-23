package tapsdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"slices"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/proof"
)

// ParentApproval identifies independently approved native package terms. Its
// committed digest excludes final witness encoding; the complete Bitcoin
// witnesses are validated separately before any child is authorized.
type ParentApproval struct {
	Package *sdk.CustomAnchorTransferPackage
	Digest  sdk.Hash
}

// ApprovedParents implements the native SDK's compact-path verification
// interfaces for one build. The SDK retains ownership of virtual transitions
// and proof-path verification. Create a fresh verifier for each operation;
// install it with builder.SetConfirmedProofVerifier before native Build, then
// call CheckPlan before Commit. Persist parent approvals in the FSM.
// Compact paths currently require isolated asset anchors in the native SDK.
type ApprovedParents struct {
	wallet   *sdk.Wallet
	chain    Chain
	source   PrevoutSource
	minConfs uint32
	parents  map[chainhash.Hash]*wire.MsgTx
	outputs  map[wire.OutPoint]*wire.TxOut
	prevouts map[wire.OutPoint]*wire.TxOut
	bases    map[[32]byte]*proof.Proof
	verified map[[32]byte]bool
	anchor   *psbt.Packet
	request  *sdk.CustomAnchorRequest
}

// NewApprovedParents binds every request input to independently agreed terms,
// authenticates fully signed parents, and checks current root funding. It does
// not build, commit, sign, publish or persist a transfer. Root funding must
// remain unspent (including mempool lookup) while pre-signing descendants. The
// chain must also implement PrevoutSource to authenticate their values/scripts.
func NewApprovedParents(ctx context.Context, wallet *sdk.Wallet, chain Chain,
	minConfs uint32, request *sdk.CustomAnchorRequest, bindings []InputBinding,
	approvals []ParentApproval) (*ApprovedParents, error) {

	if request == nil || chain == nil || minConfs == 0 {
		return nil, fmt.Errorf("request, current chain and confirmation policy are required")
	}
	source, ok := chain.(PrevoutSource)
	if !ok {
		return nil, fmt.Errorf("compact parents require authenticated Bitcoin prevouts")
	}
	if err := checkCompactRequestBudget(request); err != nil {
		return nil, err
	}
	request = request.Clone()
	g := &ApprovedParents{wallet: wallet, chain: chain, source: source,
		minConfs: minConfs, request: request,
		parents: make(map[chainhash.Hash]*wire.MsgTx), outputs: make(map[wire.OutPoint]*wire.TxOut),
		prevouts: make(map[wire.OutPoint]*wire.TxOut), bases: make(map[[32]byte]*proof.Proof),
		verified: make(map[[32]byte]bool)}
	if err := g.addParents(approvals); err != nil {
		return nil, err
	}
	var err error
	g.anchor, err = DecodePSBT(request.AnchorPSBT)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]InputBinding, len(bindings))
	for _, binding := range bindings {
		if binding.ID == "" {
			return nil, fmt.Errorf("input binding ID is required")
		}
		if _, duplicate := byID[binding.ID]; duplicate {
			return nil, fmt.Errorf("duplicate input binding")
		}
		byID[binding.ID] = binding.clone()
	}
	if err := CheckNodeVersion(ctx, wallet); err != nil {
		return nil, err
	}
	inspect := func(id string, raw []byte, path *sdk.AssetProofPath) error {
		binding, ok := byID[id]
		if !ok {
			return fmt.Errorf("input binding is missing")
		}
		delete(byID, id)
		var p *proof.Proof
		var err error
		if path != nil {
			if len(raw) != 0 {
				return fmt.Errorf("input cannot select both proof sources")
			}
			p, err = g.inspectPath(ctx, path)
		} else {
			p, err = InspectInputProof(ctx, wallet, chain, minConfs, raw, binding, true)
			if err == nil {
				g.bases[sha256.Sum256(raw)] = p
			}
		}
		if err != nil {
			return err
		}
		if err := binding.validate(p); err != nil {
			return err
		}
		return checkPathSigning(binding, g.anchor, request.SigningPlans)
	}
	for _, input := range request.Inputs {
		if err := inspect(input.ID, input.ProofFile, input.ProofPath); err != nil {
			return nil, err
		}
	}
	for _, input := range request.PassiveAssets.Packets {
		if err := inspect(input.ID, input.ProofFile, nil); err != nil {
			return nil, err
		}
	}
	if len(byID) != 0 {
		return nil, fmt.Errorf("unused input bindings")
	}
	if err := g.CheckCurrent(ctx); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *ApprovedParents) addParents(approvals []ParentApproval) error {
	spenders := make(map[wire.OutPoint]chainhash.Hash)
	for _, approval := range approvals {
		if approval.Package == nil || approval.Digest == (sdk.Hash{}) ||
			approval.Package.CommittedPackageDigest != approval.Digest {

			return fmt.Errorf("parent differs from saved committed digest")
		}
		if err := CheckSigningPolicy(approval.Package); err != nil {
			return err
		}
		parent, err := approval.Package.WithFinalAnchorPSBT(approval.Package.AnchorPsbt)
		if err != nil {
			return fmt.Errorf("parent Bitcoin authorization: %w", err)
		}
		packet, err := DecodePSBT(parent.AnchorPsbt)
		if err != nil {
			return err
		}
		tx, err := psbt.Extract(packet)
		if err != nil {
			return err
		}
		id := tx.TxHash()
		if g.parents[id] != nil {
			return fmt.Errorf("duplicate approved parent transaction")
		}
		g.parents[id] = tx
		for idx, output := range tx.TxOut {
			g.outputs[wire.OutPoint{Hash: id, Index: uint32(idx)}] = output
		}
		for idx, input := range tx.TxIn {
			point := input.PreviousOutPoint
			if previous, ok := spenders[point]; ok && previous != id {
				return fmt.Errorf("approved parents double-spend an input")
			}
			spenders[point] = id
			if packet.Inputs[idx].WitnessUtxo == nil {
				return fmt.Errorf("approved parent previous output is missing")
			}
			g.prevouts[point] = packet.Inputs[idx].WitnessUtxo
		}
	}
	return nil
}

func (g *ApprovedParents) inspectPath(ctx context.Context,
	path *sdk.AssetProofPath) (*proof.Proof, error) {

	if err := path.Validate(); err != nil {
		return nil, err
	}
	params, err := nodeParams(ctx, g.wallet)
	if err != nil {
		return nil, err
	}
	pending := []*sdk.AssetProofPath{path}
	var tip *proof.Proof
	for len(pending) != 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if err := checkProvenance(ctx, g.wallet, current.ConfirmedBaseProof); err != nil {
			return nil, err
		}
		last, err := LastProof(current.ConfirmedBaseProof)
		if err != nil {
			return nil, err
		}
		if _, _, err := isolatedRoots(last); err != nil {
			return nil, fmt.Errorf("compact parent paths require isolated asset anchors: %w", err)
		}
		g.bases[sha256.Sum256(current.ConfirmedBaseProof)] = last
		for _, step := range current.Steps {
			last, err = proof.Decode(step.TransitionProof)
			if err != nil {
				return nil, err
			}
			if last.ChallengeWitness != nil {
				return nil, fmt.Errorf("compact path contains an ownership challenge")
			}
			if err := assetpolicy.ValidateTransitionVersion(last.Version); err != nil {
				return nil, err
			}
			if height, active := assetpolicy.ActivationHeight(params); active {
				if err := assetpolicy.ValidateActivation(last, height); err != nil {
					return nil, err
				}
			}
			pending = append(pending, step.CoInputPaths...)
		}
		if current == path {
			tip = last
		}
	}
	return tip, nil
}

// VerifyConfirmedProof implements the native SDK confirmed-base boundary.
// Verification reuse is local to this factory instance and exact base bytes;
// every current-chain check repeats before and after authenticated tapd RPCs.
func (g *ApprovedParents) VerifyConfirmedProof(ctx context.Context,
	raw []byte) (*sdk.ConfirmedProofVerification, error) {

	digest := sha256.Sum256(raw)
	p := g.bases[digest]
	if p == nil {
		return nil, fmt.Errorf("compact base was not inspected")
	}
	if err := g.CheckCurrent(ctx); err != nil {
		return nil, err
	}
	if !g.verified[digest] {
		result, err := g.wallet.Client().VerifyProof(ctx, bytes.Clone(raw))
		if err != nil {
			return nil, err
		}
		if err := checkVerifiedSummary(result, p); err != nil {
			return nil, err
		}
		g.verified[digest] = true
	}
	if err := g.CheckCurrent(ctx); err != nil {
		return nil, err
	}
	return &sdk.ConfirmedProofVerification{AnchorAssetInventoryComplete: true}, nil
}

// VerifyUnconfirmedAnchor binds SDK-derived transitions to exact independently
// approved parents after executing their complete Bitcoin witnesses.
func (g *ApprovedParents) VerifyUnconfirmedAnchor(_ context.Context,
	transition sdk.UnconfirmedAnchorVerification) error {

	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(transition.AnchorTransaction)); err != nil {
		return err
	}
	parent := g.parents[tx.TxHash()]
	if parent == nil || transition.AnchorOutpoint.Txid != sdk.Hash(parent.TxHash()) ||
		transition.AnchorOutpoint.Index >= uint32(len(parent.TxOut)) {

		return fmt.Errorf("compact transition has no approved signed parent")
	}
	var actual, approved bytes.Buffer
	if err := tx.SerializeNoWitness(&actual); err != nil {
		return err
	}
	if err := parent.SerializeNoWitness(&approved); err != nil {
		return err
	}
	if !bytes.Equal(actual.Bytes(), approved.Bytes()) {
		return fmt.Errorf("compact transition differs from approved parent")
	}
	for _, input := range transition.PreviousAnchorOutpoints {
		point := SDKPoint(input)
		if !slices.ContainsFunc(parent.TxIn, func(in *wire.TxIn) bool {
			return in.PreviousOutPoint == point
		}) {

			return fmt.Errorf("compact transition uses an unapproved parent input")
		}
	}
	return nil
}

// CheckCurrent rechecks live node compatibility, current confirmed bases and
// every external Bitcoin input of the parent graph and child, including fees.
func (g *ApprovedParents) CheckCurrent(ctx context.Context) error {
	if err := CheckNodeVersion(ctx, g.wallet); err != nil {
		return err
	}
	for _, p := range g.bases {
		if err := checkConfirmation(ctx, g.chain, g.minConfs, p, true); err != nil {
			return err
		}
	}
	skip := make(map[wire.OutPoint]bool, len(g.outputs))
	for point, output := range g.outputs {
		skip[point] = true
		if previous := g.prevouts[point]; previous != nil &&
			(previous.Value != output.Value || !bytes.Equal(previous.PkScript, output.PkScript)) {

			return fmt.Errorf("parent input differs from approved graph output")
		}
	}
	for point, expected := range g.prevouts {
		if skip[point] {
			continue
		}
		actual, err := g.source.Prevout(ctx, point)
		if err != nil {
			return err
		}
		if !sameFundingOutput(expected, actual) {
			return fmt.Errorf("parent root differs from authenticated Bitcoin prevout")
		}
		unspent, err := g.chain.Unspent(ctx, point)
		if err != nil {
			return err
		}
		if !unspent {
			return fmt.Errorf("approved parent root funding is spent")
		}
	}
	for idx, input := range g.anchor.UnsignedTx.TxIn {
		if g.prevouts[input.PreviousOutPoint] != nil {
			return fmt.Errorf("child double-spends an approved parent input")
		}
		if output := g.outputs[input.PreviousOutPoint]; output != nil {
			previous := g.anchor.Inputs[idx].WitnessUtxo
			if previous == nil || previous.Value != output.Value ||
				!bytes.Equal(previous.PkScript, output.PkScript) {

				return fmt.Errorf("child input differs from approved parent output")
			}
		} else {
			actual, err := g.source.Prevout(ctx, input.PreviousOutPoint)
			if err != nil {
				return err
			}
			if !sameFundingOutput(g.anchor.Inputs[idx].WitnessUtxo, actual) {
				return fmt.Errorf("child funding differs from authenticated Bitcoin prevout")
			}
		}
	}
	return checkFundingCurrent(ctx, g.chain, g.anchor, skip)
}

func checkPathSigning(binding InputBinding, packet *psbt.Packet,
	plans []sdk.CustomAnchorInputSigningPlan) error {

	if binding.Spend == nil {
		return nil
	}
	spend := binding.Spend
	index := slices.IndexFunc(packet.UnsignedTx.TxIn, func(in *wire.TxIn) bool {
		return in.PreviousOutPoint == SDKPoint(binding.AnchorOutpoint)
	})
	if index < 0 || packet.UnsignedTx.Version < 2 ||
		packet.UnsignedTx.TxIn[index].Sequence != spend.Sequence ||
		packet.UnsignedTx.LockTime < spend.LockTime ||
		(spend.LockTime != 0 && packet.UnsignedTx.LockTime >= txscript.LockTimeThreshold) {

		return fmt.Errorf("contract sequence and locktime must be set before native Build")
	}
	leaf := sdk.Hash(txscript.NewBaseTapLeaf(spend.LeafScript).TapHash())
	for _, plan := range plans {
		if plan.InputIndex == uint32(index) && plan.ScriptPath != nil &&
			plan.ScriptPath.LeafHash == leaf && slices.Equal(plan.ScriptPath.RequiredSigners,
			[]sdk.XOnlyPubKey{sdk.XOnlyPubKey(spend.RequiredSigner)}) {

			return nil
		}
	}
	return fmt.Errorf("contract signing plan differs from agreed spend")
}

func checkCompactRequestBudget(request *sdk.CustomAnchorRequest) error {
	var total int
	add := func(size int) error {
		if size > assetpolicy.MaxProvenanceBytes-total {
			return fmt.Errorf("compact request histories exceed size budget")
		}
		total += size
		return nil
	}
	for _, input := range request.Inputs {
		if input.ProofPath == nil {
			if err := add(len(input.ProofFile)); err != nil {
				return err
			}
			continue
		}
		raw, err := input.ProofPath.MarshalBinary()
		if err != nil {
			return err
		}
		if err := add(len(raw)); err != nil {
			return err
		}
	}
	for _, input := range request.PassiveAssets.Packets {
		if err := add(len(input.ProofFile)); err != nil {
			return err
		}
	}
	return nil
}

// CheckPlan binds the native built plan to the exact request inspected by this
// verifier, then repeats current funding checks. Call before native Commit; a
// different request must obtain fresh independent bindings and a new verifier.
func (g *ApprovedParents) CheckPlan(ctx context.Context, plan *sdk.CustomAnchorPlan) error {
	if plan == nil || !reflect.DeepEqual(plan.Request(), g.request) {
		return fmt.Errorf("native plan differs from inspected compact request")
	}
	return g.CheckCurrent(ctx)
}

func sameFundingOutput(expected, actual *wire.TxOut) bool {
	return expected != nil && actual != nil && actual.Value >= 0 &&
		expected.Value == actual.Value && bytes.Equal(expected.PkScript, actual.PkScript)
}
