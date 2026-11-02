package tapsdk

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	btcpsbt "github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

var customAnchorSigningRequestDomain = []byte(
	"tap-sdk/custom-anchor/signing-request/v1",
)

// CustomAnchorSigningMode identifies the selected external anchor input spend
// path.
type CustomAnchorSigningMode uint8

const (
	// CustomAnchorSigningModeUnknown is not a valid signing plan.
	CustomAnchorSigningModeUnknown CustomAnchorSigningMode = iota

	// CustomAnchorSigningModeKeyPath selects a single-signer Taproot key
	// spend.
	CustomAnchorSigningModeKeyPath

	// CustomAnchorSigningModeMuSig2 selects a final aggregate MuSig2
	// Taproot key spend.
	CustomAnchorSigningModeMuSig2

	// CustomAnchorSigningModeScriptPath selects one exact Taproot leaf.
	CustomAnchorSigningModeScriptPath
)

// CustomAnchorInputSigningPlan selects exactly one external spending path for
// one committed anchor input. Pointer variants model a tagged union; callers
// must set exactly one variant.
type CustomAnchorInputSigningPlan struct {
	// InputIndex is the input in the committed anchor transaction.
	InputIndex uint32 `json:"input_index"`

	// KeyPath selects a single-signer Taproot key spend.
	KeyPath *CustomAnchorKeyPathSigningPlan `json:"key_path,omitempty"`

	// MuSig2 selects a final aggregate MuSig2 Taproot key spend.
	MuSig2 *CustomAnchorMuSig2SigningPlan `json:"musig2,omitempty"`

	// ScriptPath selects one exact Taproot leaf spend.
	ScriptPath *CustomAnchorScriptPathSigningPlan `json:"script_path,omitempty"`

	// CallerSigned declares an input whose witness the caller supplies
	// out of band. The SDK derives no signing request and the backend
	// must not sign it.
	CallerSigned *CustomAnchorCallerSignedPlan `json:"caller_signed,omitempty"`
}

// CustomAnchorKeyPathSigningPlan identifies the internal key that must
// authorize a single-signer Taproot key spend.
type CustomAnchorKeyPathSigningPlan struct {
	// Signer is the x-only internal key controlled by the signer.
	Signer XOnlyPubKey `json:"signer"`
}

// CustomAnchorMuSig2SigningPlan identifies an ordered MuSig2 participant set.
// Participant order is significant and is preserved in the signing request.
type CustomAnchorMuSig2SigningPlan struct {
	// Participants are the ordered 33-byte compressed keys used for key
	// aggregation. Full keys preserve Y parity, so any cosigner key can
	// participate; callers whose aggregation sorts keys declare them
	// pre-sorted.
	Participants []PubKey `json:"participants"`

	// SessionContext is caller-defined public context used to distinguish
	// signing sessions. It must never contain a secret nonce.
	SessionContext []byte `json:"session_context"`
}

// CustomAnchorCallerSignedPlan declares an anchor input signed entirely by
// the caller, outside the SDK's signing-request flow: typically a funding
// input from the caller's own on-chain wallet on a caller-funded anchor.
type CustomAnchorCallerSignedPlan struct{}

// CustomAnchorScriptPathSigningPlan identifies one exact committed Taproot
// leaf and any external keys expected to contribute witness signatures. The
// first implementation rejects leaves containing OP_CODESEPARATOR and final
// witnesses containing an annex.
type CustomAnchorScriptPathSigningPlan struct {
	// LeafHash is the TapLeaf hash of the selected script path.
	LeafHash Hash `json:"leaf_hash"`

	// RequiredSigners are the x-only keys expected to contribute witness
	// signatures. An OP_TRUE-style leaf can leave this empty.
	RequiredSigners []XOnlyPubKey `json:"required_signers,omitempty"`
}

// CustomAnchorSigningRequests groups the typed requests derived from a sealed
// package and its explicit per-input signing plans.
type CustomAnchorSigningRequests struct {
	KeyPath    []CustomAnchorKeyPathSigningRequest
	MuSig2     []CustomAnchorMuSig2SigningRequest
	ScriptPath []CustomAnchorScriptPathSigningRequest
}

// CustomAnchorKeyPathSigningRequest contains all SDK-owned data required to
// review and sign a single-signer Taproot key spend.
type CustomAnchorKeyPathSigningRequest struct {
	ID                Hash
	PackageDigest     Hash
	InputIndex        uint32
	PrevOut           Outpoint
	PrevOutValueSat   int64
	PrevOutScript     []byte
	SighashType       uint32
	Sighash           Hash
	InternalKey       XOnlyPubKey
	TaprootMerkleRoot []byte
	Signer            XOnlyPubKey
}

// CustomAnchorMuSig2SigningRequest contains the transaction digest and ordered
// participant set for an external MuSig2 session. Nonce generation, exchange,
// and session orchestration remain the signer's responsibility.
type CustomAnchorMuSig2SigningRequest struct {
	ID                Hash
	PackageDigest     Hash
	InputIndex        uint32
	PrevOut           Outpoint
	PrevOutValueSat   int64
	PrevOutScript     []byte
	SighashType       uint32
	Sighash           Hash
	InternalKey       XOnlyPubKey
	TaprootMerkleRoot []byte
	Participants      []PubKey
	SessionContext    []byte
}

// CustomAnchorScriptPathSigningRequest contains one exact Taproot leaf and the
// data needed to construct and review its final witness stack.
type CustomAnchorScriptPathSigningRequest struct {
	ID                Hash
	PackageDigest     Hash
	InputIndex        uint32
	PrevOut           Outpoint
	PrevOutValueSat   int64
	PrevOutScript     []byte
	SighashType       uint32
	Sighash           Hash
	InternalKey       XOnlyPubKey
	TaprootMerkleRoot []byte
	LeafHash          Hash
	LeafVersion       uint8
	LeafScript        []byte
	ControlBlock      []byte
	RequiredSigners   []XOnlyPubKey
}

type customAnchorSigningContext struct {
	inputIndex        uint32
	prevOut           Outpoint
	prevOutValueSat   int64
	prevOutScript     []byte
	sighashType       uint32
	sighash           Hash
	internalKey       XOnlyPubKey
	taprootMerkleRoot []byte
}

// SigningRequests derives deterministic, digest-bound requests from the
// committed anchor PSBT. It fails closed if any input is not classified as
// either externally signed or backend-managed, or if a plan is ambiguous or
// incompatible with the PSBT.
func (p *CustomAnchorTransferPackage) SigningRequests() (
	*CustomAnchorSigningRequests, error) {

	if err := p.Validate(); err != nil {
		return nil, err
	}

	packet, err := decodeAnchorPSBT(p.CommittedAnchorPsbt)
	if err != nil {
		return nil, fmt.Errorf("anchor PSBT: %w", err)
	}

	return customAnchorSigningRequests(
		packet, p.SigningPlans, p.BackendManagedInputIndices,
		p.CommittedPackageDigest,
	)
}

func customAnchorSigningRequests(packet *btcpsbt.Packet,
	signingPlans []CustomAnchorInputSigningPlan,
	backendManaged []uint32, packageDigest Hash) (
	*CustomAnchorSigningRequests, error) {

	if packet == nil || packet.UnsignedTx == nil {
		return nil, fmt.Errorf("anchor PSBT is required")
	}
	if err := validateCustomAnchorSigningPlans(
		signingPlans, backendManaged,
		uint32(len(packet.UnsignedTx.TxIn)),
	); err != nil {
		return nil, err
	}
	prevOutFetcher, prevOuts, err := anchorPrevOuts(packet)
	if err != nil {
		return nil, err
	}
	sigHashes := txscript.NewTxSigHashes(
		packet.UnsignedTx, prevOutFetcher,
	)

	plans := cloneCustomAnchorSigningPlans(signingPlans)
	sort.Slice(plans, func(i, j int) bool {
		return plans[i].InputIndex < plans[j].InputIndex
	})

	requests := &CustomAnchorSigningRequests{
		KeyPath: make([]CustomAnchorKeyPathSigningRequest, 0),
		MuSig2:  make([]CustomAnchorMuSig2SigningRequest, 0),
		ScriptPath: make(
			[]CustomAnchorScriptPathSigningRequest, 0,
		),
	}
	for _, plan := range plans {
		inputIndex := int(plan.InputIndex)
		pInput := &packet.Inputs[inputIndex]
		prevOut := prevOuts[inputIndex]

		switch {
		case plan.KeyPath != nil:
			context, err := keyPathSigningContext(
				packet, pInput, prevOutFetcher, sigHashes,
				plan.InputIndex,
			)
			if err != nil {
				return nil, err
			}
			if context.internalKey != plan.KeyPath.Signer {
				return nil, fmt.Errorf(
					"input %d key-path signer does not match "+
						"the committed internal key", inputIndex,
				)
			}

			requests.KeyPath = append(
				requests.KeyPath,
				CustomAnchorKeyPathSigningRequest{
					ID: customAnchorSigningRequestID(
						packageDigest,
						CustomAnchorSigningModeKeyPath,
						plan.InputIndex,
					),
					PackageDigest:   packageDigest,
					InputIndex:      plan.InputIndex,
					PrevOut:         context.prevOut,
					PrevOutValueSat: context.prevOutValueSat,
					PrevOutScript: cloneBytes(
						context.prevOutScript,
					),
					SighashType: context.sighashType,
					Sighash:     context.sighash,
					InternalKey: context.internalKey,
					TaprootMerkleRoot: cloneBytes(
						context.taprootMerkleRoot,
					),
					Signer: plan.KeyPath.Signer,
				},
			)

		case plan.MuSig2 != nil:
			context, err := keyPathSigningContext(
				packet, pInput, prevOutFetcher, sigHashes,
				plan.InputIndex,
			)
			if err != nil {
				return nil, err
			}
			if err := validateMuSig2Aggregate(
				context.internalKey, plan.MuSig2.Participants,
			); err != nil {
				return nil, fmt.Errorf(
					"input %d MuSig2 plan: %w", inputIndex,
					err,
				)
			}

			requests.MuSig2 = append(
				requests.MuSig2,
				CustomAnchorMuSig2SigningRequest{
					ID: customAnchorSigningRequestID(
						packageDigest,
						CustomAnchorSigningModeMuSig2,
						plan.InputIndex,
					),
					PackageDigest:   packageDigest,
					InputIndex:      plan.InputIndex,
					PrevOut:         context.prevOut,
					PrevOutValueSat: context.prevOutValueSat,
					PrevOutScript: cloneBytes(
						context.prevOutScript,
					),
					SighashType: context.sighashType,
					Sighash:     context.sighash,
					InternalKey: context.internalKey,
					TaprootMerkleRoot: cloneBytes(
						context.taprootMerkleRoot,
					),
					Participants: append(
						[]PubKey(nil),
						plan.MuSig2.Participants...,
					),
					SessionContext: cloneBytes(
						plan.MuSig2.SessionContext,
					),
				},
			)

		case plan.ScriptPath != nil:
			request, err := scriptPathSigningRequest(
				packageDigest, packet, pInput,
				prevOutFetcher, sigHashes, prevOut, plan,
			)
			if err != nil {
				return nil, err
			}

			requests.ScriptPath = append(
				requests.ScriptPath, *request,
			)

		case plan.CallerSigned != nil:
			// The caller supplies this input's witness out of
			// band; no signing request is derived.
		}
	}

	return requests, nil
}

// ApplyKeyPathSignature verifies and immutably applies either a single-signer
// or final aggregate MuSig2 Schnorr signature. The original package and all
// caller-owned buffers remain unchanged.
func (p *CustomAnchorTransferPackage) ApplyKeyPathSignature(requestID Hash,
	signature []byte) (*CustomAnchorTransferPackage, error) {

	requests, err := p.SigningRequests()
	if err != nil {
		return nil, err
	}

	var request *CustomAnchorKeyPathSigningRequest
	for i := range requests.KeyPath {
		if requests.KeyPath[i].ID == requestID {
			request = &requests.KeyPath[i]
			break
		}
	}

	var musigRequest *CustomAnchorMuSig2SigningRequest
	if request == nil {
		for i := range requests.MuSig2 {
			if requests.MuSig2[i].ID == requestID {
				musigRequest = &requests.MuSig2[i]
				break
			}
		}
	}
	if request == nil && musigRequest == nil {
		return nil, fmt.Errorf("unknown key-path signing request")
	}

	var (
		inputIndex    uint32
		sighashType   uint32
		sighash       Hash
		prevOutScript []byte
	)
	if request != nil {
		inputIndex = request.InputIndex
		sighashType = request.SighashType
		sighash = request.Sighash
		prevOutScript = request.PrevOutScript
	} else {
		inputIndex = musigRequest.InputIndex
		sighashType = musigRequest.SighashType
		sighash = musigRequest.Sighash
		prevOutScript = musigRequest.PrevOutScript
	}

	if err := verifyTaprootKeyPathSignature(
		signature, sighashType, sighash, prevOutScript,
	); err != nil {
		return nil, err
	}

	clone := p.Clone()
	packet, err := decodeAnchorPSBT(clone.AnchorPsbt)
	if err != nil {
		return nil, fmt.Errorf("anchor PSBT: %w", err)
	}
	pInput := &packet.Inputs[inputIndex]
	if len(pInput.FinalScriptWitness) != 0 ||
		len(pInput.FinalScriptSig) != 0 {

		return nil, fmt.Errorf("input %d is already finalized", inputIndex)
	}
	if len(pInput.TaprootKeySpendSig) != 0 && !bytes.Equal(
		pInput.TaprootKeySpendSig, signature,
	) {

		return nil, fmt.Errorf(
			"input %d already has a different key-path signature",
			inputIndex,
		)
	}
	pInput.TaprootKeySpendSig = cloneBytes(signature)

	anchorPsbt, err := serializeAnchorPSBT(packet)
	if err != nil {
		return nil, err
	}
	clone.AnchorPsbt = anchorPsbt
	if err := clone.refreshPackageDigest(); err != nil {
		return nil, err
	}
	if err := clone.Validate(); err != nil {
		return nil, err
	}

	return clone, nil
}

// ApplyScriptPathWitness verifies and immutably applies the unlocking elements
// for one selected script path. The committed leaf script and control block
// are appended by the SDK and cannot be replaced by the caller.
func (p *CustomAnchorTransferPackage) ApplyScriptPathWitness(
	requestID Hash, witnessElements [][]byte) (
	*CustomAnchorTransferPackage, error) {

	requests, err := p.SigningRequests()
	if err != nil {
		return nil, err
	}

	var request *CustomAnchorScriptPathSigningRequest
	for i := range requests.ScriptPath {
		if requests.ScriptPath[i].ID == requestID {
			request = &requests.ScriptPath[i]
			break
		}
	}
	if request == nil {
		return nil, fmt.Errorf("unknown script-path signing request")
	}

	clone := p.Clone()
	packet, err := decodeAnchorPSBT(clone.AnchorPsbt)
	if err != nil {
		return nil, fmt.Errorf("anchor PSBT: %w", err)
	}
	inputIndex := int(request.InputIndex)
	pInput := &packet.Inputs[inputIndex]
	if len(pInput.TaprootKeySpendSig) != 0 {
		return nil, fmt.Errorf(
			"input %d already has a key-path signature", inputIndex,
		)
	}

	witness := cloneByteSlices(witnessElements)
	witness = append(witness, cloneBytes(request.LeafScript))
	witness = append(witness, cloneBytes(request.ControlBlock))

	prevOutFetcher, prevOuts, err := anchorPrevOuts(packet)
	if err != nil {
		return nil, err
	}
	sigHashes := txscript.NewTxSigHashes(
		packet.UnsignedTx, prevOutFetcher,
	)
	if err := verifyScriptPathWitness(
		packet.UnsignedTx, inputIndex, prevOuts[inputIndex],
		prevOutFetcher, sigHashes, witness,
	); err != nil {
		return nil, err
	}

	var serializedWitness bytes.Buffer
	if err := btcpsbt.WriteTxWitness(
		&serializedWitness, witness,
	); err != nil {
		return nil, fmt.Errorf("serialize script-path witness: %w", err)
	}
	if len(pInput.FinalScriptWitness) != 0 && !bytes.Equal(
		pInput.FinalScriptWitness, serializedWitness.Bytes(),
	) {

		return nil, fmt.Errorf(
			"input %d already has a different final witness",
			inputIndex,
		)
	}
	pInput.FinalScriptWitness = cloneBytes(serializedWitness.Bytes())

	anchorPsbt, err := serializeAnchorPSBT(packet)
	if err != nil {
		return nil, err
	}
	clone.AnchorPsbt = anchorPsbt
	if err := clone.refreshPackageDigest(); err != nil {
		return nil, err
	}
	if err := clone.Validate(); err != nil {
		return nil, err
	}

	return clone, nil
}

// VerifyFinalAnchorPSBT verifies that a signed or finalized PSBT contains the
// exact committed unsigned transaction. A fully finalized PSBT must also use
// every sealed external signing path and pass consensus witness validation.
func (p *CustomAnchorTransferPackage) VerifyFinalAnchorPSBT(
	finalAnchorPsbt []byte) error {

	if err := p.Validate(); err != nil {
		return err
	}

	_, finalPacket, err := sanitizeFinalAnchorPSBT(
		p.CommittedAnchorPsbt, finalAnchorPsbt,
	)
	if err != nil {
		return err
	}

	// A partially signed PSBT can only be checked for transaction and PSBT
	// metadata equality here. Once all inputs are finalized, also enforce the
	// sealed signing path and execute every final witness.
	if !allAnchorInputsFinalized(finalPacket) {
		return nil
	}
	finalTx, err := btcpsbt.Extract(finalPacket)
	if err != nil {
		return fmt.Errorf("final anchor PSBT is not finalized: %w", err)
	}
	if err := p.verifyFinalAnchorSigningPlans(finalTx); err != nil {
		return err
	}

	return verifyFinalAnchorWitnesses(finalPacket, finalTx)
}

// WithFinalAnchorPSBT verifies and stores a fully finalized anchor PSBT on an
// immutable package clone. Each external input must use its sealed path and
// exact sighash, while backend-managed inputs remain consensus-validated. All
// non-final PSBT metadata is restored from the committed snapshot before the
// current package digest is refreshed.
func (p *CustomAnchorTransferPackage) WithFinalAnchorPSBT(
	finalAnchorPsbt []byte) (*CustomAnchorTransferPackage, error) {

	if err := p.Validate(); err != nil {
		return nil, err
	}
	sanitized, finalPacket, err := sanitizeFinalAnchorPSBT(
		p.CommittedAnchorPsbt, finalAnchorPsbt,
	)
	if err != nil {
		return nil, err
	}
	finalTx, err := btcpsbt.Extract(finalPacket)
	if err != nil {
		return nil, fmt.Errorf("final anchor PSBT is not finalized: %w", err)
	}
	if err := p.verifyFinalAnchorSigningPlans(finalTx); err != nil {
		return nil, err
	}
	if err := verifyFinalAnchorWitnesses(finalPacket, finalTx); err != nil {
		return nil, err
	}

	clone := p.Clone()
	clone.AnchorPsbt = sanitized
	if err := clone.refreshPackageDigest(); err != nil {
		return nil, err
	}
	if err := clone.Validate(); err != nil {
		return nil, err
	}

	return clone, nil
}

func (p *CustomAnchorTransferPackage) verifyFinalAnchorSigningPlans(
	finalTx *wire.MsgTx) error {

	requests, err := p.SigningRequests()
	if err != nil {
		return fmt.Errorf("derive sealed signing requests: %w", err)
	}

	keyPath := make(
		map[uint32]*CustomAnchorKeyPathSigningRequest,
		len(requests.KeyPath),
	)
	for idx := range requests.KeyPath {
		request := &requests.KeyPath[idx]
		keyPath[request.InputIndex] = request
	}
	muSig2 := make(
		map[uint32]*CustomAnchorMuSig2SigningRequest,
		len(requests.MuSig2),
	)
	for idx := range requests.MuSig2 {
		request := &requests.MuSig2[idx]
		muSig2[request.InputIndex] = request
	}
	scriptPath := make(
		map[uint32]*CustomAnchorScriptPathSigningRequest,
		len(requests.ScriptPath),
	)
	for idx := range requests.ScriptPath {
		request := &requests.ScriptPath[idx]
		scriptPath[request.InputIndex] = request
	}
	backendManaged := make(
		map[uint32]struct{}, len(p.BackendManagedInputIndices),
	)
	for _, inputIndex := range p.BackendManagedInputIndices {
		backendManaged[inputIndex] = struct{}{}
	}
	callerSigned := make(map[uint32]struct{}, len(p.SigningPlans))
	for _, plan := range p.SigningPlans {
		if plan.CallerSigned != nil {
			callerSigned[plan.InputIndex] = struct{}{}
		}
	}

	for idx := range finalTx.TxIn {
		inputIndex := uint32(idx)
		if _, ok := backendManaged[inputIndex]; ok {
			continue
		}

		witness := finalTx.TxIn[idx].Witness
		if customAnchorWitnessHasAnnex(witness) {
			return fmt.Errorf(
				"final anchor input %d annexes are unsupported", idx,
			)
		}

		switch {
		case keyPath[inputIndex] != nil:
			request := keyPath[inputIndex]
			if err := verifyFinalKeyPathWitness(
				idx, witness, request.SighashType, request.Sighash,
				request.PrevOutScript,
			); err != nil {
				return err
			}

		case muSig2[inputIndex] != nil:
			request := muSig2[inputIndex]
			if err := verifyFinalKeyPathWitness(
				idx, witness, request.SighashType, request.Sighash,
				request.PrevOutScript,
			); err != nil {
				return err
			}

		case scriptPath[inputIndex] != nil:
			request := scriptPath[inputIndex]
			if err := verifyFinalScriptPathWitness(
				idx, witness, request,
			); err != nil {
				return err
			}

		case hasCallerSignedPlan(callerSigned, inputIndex):
			if len(witness) == 0 {
				return fmt.Errorf(
					"final anchor input %d misses its caller-signed "+
						"witness", idx,
				)
			}

		default:
			return fmt.Errorf(
				"final anchor input %d has no sealed signing plan", idx,
			)
		}
	}

	return nil
}

// hasCallerSignedPlan reports whether the input carries a caller-signed
// plan.
func hasCallerSignedPlan(callerSigned map[uint32]struct{},
	inputIndex uint32) bool {

	_, ok := callerSigned[inputIndex]
	return ok
}

func verifyFinalKeyPathWitness(inputIndex int, witness wire.TxWitness,
	sighashType uint32, sighash Hash, prevOutScript []byte) error {

	if len(witness) != 1 {
		return fmt.Errorf(
			"final anchor input %d must use its sealed key path",
			inputIndex,
		)
	}
	if err := verifyTaprootKeyPathSignature(
		witness[0], sighashType, sighash, prevOutScript,
	); err != nil {
		return fmt.Errorf(
			"final anchor input %d sealed key-path signature: %w",
			inputIndex, err,
		)
	}

	return nil
}

func verifyFinalScriptPathWitness(inputIndex int, witness wire.TxWitness,
	request *CustomAnchorScriptPathSigningRequest) error {

	if len(witness) < 2 {
		return fmt.Errorf(
			"final anchor input %d must use its sealed script path",
			inputIndex,
		)
	}
	leafScript := witness[len(witness)-2]
	controlBlock := witness[len(witness)-1]
	if !bytes.Equal(leafScript, request.LeafScript) ||
		!bytes.Equal(controlBlock, request.ControlBlock) {

		return fmt.Errorf(
			"final anchor input %d does not use the sealed script leaf "+
				"and control block", inputIndex,
		)
	}
	if err := validateCustomAnchorTapscript(request.LeafScript); err != nil {
		return fmt.Errorf("final anchor input %d: %w", inputIndex, err)
	}
	if err := verifyRequiredScriptPathSignatures(
		witness[:len(witness)-2], request,
	); err != nil {
		return fmt.Errorf(
			"final anchor input %d sealed script-path signatures: %w",
			inputIndex, err,
		)
	}

	return nil
}

func verifyRequiredScriptPathSignatures(witnessElements wire.TxWitness,
	request *CustomAnchorScriptPathSigningRequest) error {

	matchedSigners := make(map[int]struct{}, len(request.RequiredSigners))
	for witnessIndex, element := range witnessElements {
		if len(element) != schnorr.SignatureSize &&
			len(element) != schnorr.SignatureSize+1 {

			continue
		}

		matched := false
		for signerIndex, signer := range request.RequiredSigners {
			if verifyTaprootScriptPathSignature(
				element, request.SighashType, request.Sighash,
				signer,
			) != nil {

				continue
			}

			matchedSigners[signerIndex] = struct{}{}
			matched = true
			break
		}
		if !matched {
			return fmt.Errorf(
				"signature-shaped witness element %d does not match a "+
					"declared signer and the exact sealed sighash",
				witnessIndex,
			)
		}
	}
	for signerIndex := range request.RequiredSigners {
		if _, ok := matchedSigners[signerIndex]; !ok {
			return fmt.Errorf(
				"required signer %d did not authorize the exact sealed "+
					"sighash", signerIndex,
			)
		}
	}

	return nil
}

func allAnchorInputsFinalized(packet *btcpsbt.Packet) bool {
	for idx := range packet.Inputs {
		if len(packet.Inputs[idx].FinalScriptSig) == 0 &&
			len(packet.Inputs[idx].FinalScriptWitness) == 0 {

			return false
		}
	}

	return true
}

func customAnchorWitnessHasAnnex(witness wire.TxWitness) bool {
	if len(witness) < 2 {
		return false
	}
	lastElement := witness[len(witness)-1]

	return len(lastElement) > 0 && lastElement[0] == txscript.TaprootAnnexTag
}

func validateCustomAnchorSigningPlans(
	plans []CustomAnchorInputSigningPlan, backendManaged []uint32,
	inputCount uint32) error {

	if uint32(len(plans)+len(backendManaged)) != inputCount {
		return fmt.Errorf(
			"signing plans and backend-managed inputs must cover all %d "+
				"anchor inputs", inputCount,
		)
	}

	seen := make(map[uint32]struct{}, len(plans))
	for i := range plans {
		plan := &plans[i]
		if plan.InputIndex >= inputCount {
			return fmt.Errorf(
				"signing plan %d input index is out of range", i,
			)
		}
		if _, ok := seen[plan.InputIndex]; ok {
			return fmt.Errorf(
				"duplicate signing plan for input %d", plan.InputIndex,
			)
		}
		seen[plan.InputIndex] = struct{}{}

		variants := 0
		if plan.KeyPath != nil {
			variants++
			if err := validateCustomAnchorXOnlyKey(
				"key-path signer", plan.KeyPath.Signer,
			); err != nil {
				return fmt.Errorf("signing plan %d: %w", i, err)
			}
		}
		if plan.MuSig2 != nil {
			variants++
			if len(plan.MuSig2.Participants) < 2 {
				return fmt.Errorf(
					"signing plan %d MuSig2 requires at least two "+
						"participants", i,
				)
			}
			if len(plan.MuSig2.SessionContext) == 0 {
				return fmt.Errorf(
					"signing plan %d MuSig2 session context is "+
						"required", i,
				)
			}
			if err := validateCustomAnchorPubKeys(
				"MuSig2 participant", plan.MuSig2.Participants,
			); err != nil {
				return fmt.Errorf("signing plan %d: %w", i, err)
			}
		}
		if plan.ScriptPath != nil {
			variants++
			if plan.ScriptPath.LeafHash == (Hash{}) {
				return fmt.Errorf(
					"signing plan %d script-path leaf hash is "+
						"required", i,
				)
			}
			if err := validateCustomAnchorXOnlyKeys(
				"script-path signer",
				plan.ScriptPath.RequiredSigners,
			); err != nil {
				return fmt.Errorf("signing plan %d: %w", i, err)
			}
		}
		if plan.CallerSigned != nil {
			variants++
		}
		if variants != 1 {
			return fmt.Errorf(
				"signing plan %d must set exactly one spending path", i,
			)
		}
	}

	for i, inputIndex := range backendManaged {
		if inputIndex >= inputCount {
			return fmt.Errorf(
				"backend-managed input %d index is out of range", i,
			)
		}
		if _, ok := seen[inputIndex]; ok {
			return fmt.Errorf(
				"anchor input %d is classified more than once", inputIndex,
			)
		}
		seen[inputIndex] = struct{}{}
	}
	for inputIndex := range inputCount {
		if _, ok := seen[inputIndex]; !ok {
			return fmt.Errorf("anchor input %d is not classified", inputIndex)
		}
	}

	return nil
}

func cloneCustomAnchorSigningPlans(
	plans []CustomAnchorInputSigningPlan) []CustomAnchorInputSigningPlan {

	if plans == nil {
		return nil
	}

	clone := make([]CustomAnchorInputSigningPlan, len(plans))
	for i := range plans {
		clone[i] = plans[i]
		if plans[i].KeyPath != nil {
			keyPath := *plans[i].KeyPath
			clone[i].KeyPath = &keyPath
		}
		if plans[i].MuSig2 != nil {
			muSig2 := *plans[i].MuSig2
			muSig2.Participants = append(
				[]PubKey(nil), plans[i].MuSig2.Participants...,
			)
			muSig2.SessionContext = cloneBytes(
				plans[i].MuSig2.SessionContext,
			)
			clone[i].MuSig2 = &muSig2
		}
		if plans[i].CallerSigned != nil {
			callerSigned := *plans[i].CallerSigned
			clone[i].CallerSigned = &callerSigned
		}
		if plans[i].ScriptPath != nil {
			scriptPath := *plans[i].ScriptPath
			scriptPath.RequiredSigners = append(
				[]XOnlyPubKey(nil),
				plans[i].ScriptPath.RequiredSigners...,
			)
			clone[i].ScriptPath = &scriptPath
		}
	}

	return clone
}

func keyPathSigningContext(packet *btcpsbt.Packet,
	pInput *btcpsbt.PInput, prevOutFetcher *txscript.MultiPrevOutFetcher,
	sigHashes *txscript.TxSigHashes, inputIndex uint32) (
	*customAnchorSigningContext, error) {

	prevOut := prevOutFetcher.FetchPrevOutput(
		packet.UnsignedTx.TxIn[inputIndex].PreviousOutPoint,
	)
	if prevOut == nil {
		return nil, fmt.Errorf("input %d previous output is missing", inputIndex)
	}
	if !txscript.IsPayToTaproot(prevOut.PkScript) {
		return nil, fmt.Errorf("input %d is not pay-to-taproot", inputIndex)
	}
	if len(pInput.TaprootInternalKey) != schnorr.PubKeyBytesLen {
		return nil, fmt.Errorf(
			"input %d taproot internal key is required", inputIndex,
		)
	}
	if len(pInput.TaprootMerkleRoot) != 0 &&
		len(pInput.TaprootMerkleRoot) != 32 {

		return nil, fmt.Errorf(
			"input %d taproot merkle root must be 32 bytes",
			inputIndex,
		)
	}

	internalKey, err := schnorr.ParsePubKey(pInput.TaprootInternalKey)
	if err != nil {
		return nil, fmt.Errorf(
			"input %d invalid taproot internal key: %w", inputIndex,
			err,
		)
	}
	outputKey := txscript.ComputeTaprootOutputKey(
		internalKey, pInput.TaprootMerkleRoot,
	)
	_, witnessProgram, err := txscript.ExtractWitnessProgramInfo(
		prevOut.PkScript,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"input %d taproot witness program: %w", inputIndex, err,
		)
	}
	if !bytes.Equal(schnorr.SerializePubKey(outputKey), witnessProgram) {
		return nil, fmt.Errorf(
			"input %d internal key and merkle root do not match "+
				"the previous output", inputIndex,
		)
	}

	sighashType := pInput.SighashType
	sighashBytes, err := txscript.CalcTaprootSignatureHash(
		sigHashes, sighashType, packet.UnsignedTx, int(inputIndex),
		prevOutFetcher,
	)
	if err != nil {
		return nil, fmt.Errorf("input %d key-path sighash: %w", inputIndex, err)
	}

	return newCustomAnchorSigningContext(
		packet, inputIndex, prevOut, sighashType, sighashBytes,
		pInput.TaprootInternalKey, pInput.TaprootMerkleRoot,
	), nil
}

func scriptPathSigningRequest(packageDigest Hash, packet *btcpsbt.Packet,
	pInput *btcpsbt.PInput, prevOutFetcher *txscript.MultiPrevOutFetcher,
	sigHashes *txscript.TxSigHashes, prevOut *wire.TxOut,
	plan CustomAnchorInputSigningPlan) (
	*CustomAnchorScriptPathSigningRequest, error) {

	inputIndex := int(plan.InputIndex)
	var matches []*btcpsbt.TaprootTapLeafScript
	for _, leaf := range pInput.TaprootLeafScript {
		tapLeaf := txscript.NewTapLeaf(leaf.LeafVersion, leaf.Script)
		leafHash := tapLeaf.TapHash()
		if bytes.Equal(leafHash[:], plan.ScriptPath.LeafHash[:]) {
			matches = append(matches, leaf)
		}
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf(
			"input %d script-path leaf matched %d PSBT entries; "+
				"exactly one is required", inputIndex, len(matches),
		)
	}
	leaf := matches[0]
	if err := validateCustomAnchorTapscript(leaf.Script); err != nil {
		return nil, fmt.Errorf("input %d: %w", inputIndex, err)
	}

	controlBlock, err := txscript.ParseControlBlock(leaf.ControlBlock)
	if err != nil {
		return nil, fmt.Errorf(
			"input %d script-path control block: %w", inputIndex, err,
		)
	}
	if controlBlock.LeafVersion != leaf.LeafVersion {
		return nil, fmt.Errorf(
			"input %d control block leaf version mismatch", inputIndex,
		)
	}
	_, witnessProgram, err := txscript.ExtractWitnessProgramInfo(
		prevOut.PkScript,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"input %d taproot witness program: %w", inputIndex, err,
		)
	}
	if err := txscript.VerifyTaprootLeafCommitment(
		controlBlock, witnessProgram, leaf.Script,
	); err != nil {
		return nil, fmt.Errorf(
			"input %d script-path commitment: %w", inputIndex, err,
		)
	}

	merkleRoot := controlBlock.RootHash(leaf.Script)
	internalKeyBytes := schnorr.SerializePubKey(controlBlock.InternalKey)
	if len(pInput.TaprootInternalKey) != 0 && !bytes.Equal(
		pInput.TaprootInternalKey, internalKeyBytes,
	) {

		return nil, fmt.Errorf(
			"input %d script-path internal key mismatch", inputIndex,
		)
	}
	if len(pInput.TaprootMerkleRoot) != 0 && !bytes.Equal(
		pInput.TaprootMerkleRoot, merkleRoot,
	) {

		return nil, fmt.Errorf(
			"input %d script-path merkle root mismatch", inputIndex,
		)
	}

	tapLeaf := txscript.NewTapLeaf(leaf.LeafVersion, leaf.Script)
	sighashBytes, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, pInput.SighashType, packet.UnsignedTx, inputIndex,
		prevOutFetcher, tapLeaf,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"input %d script-path sighash: %w", inputIndex, err,
		)
	}
	context := newCustomAnchorSigningContext(
		packet, plan.InputIndex, prevOut, pInput.SighashType,
		sighashBytes, internalKeyBytes, merkleRoot,
	)

	return &CustomAnchorScriptPathSigningRequest{
		ID: customAnchorSigningRequestID(
			packageDigest, CustomAnchorSigningModeScriptPath,
			plan.InputIndex,
		),
		PackageDigest:     packageDigest,
		InputIndex:        plan.InputIndex,
		PrevOut:           context.prevOut,
		PrevOutValueSat:   context.prevOutValueSat,
		PrevOutScript:     cloneBytes(context.prevOutScript),
		SighashType:       context.sighashType,
		Sighash:           context.sighash,
		InternalKey:       context.internalKey,
		TaprootMerkleRoot: cloneBytes(context.taprootMerkleRoot),
		LeafHash:          plan.ScriptPath.LeafHash,
		LeafVersion:       uint8(leaf.LeafVersion),
		LeafScript:        cloneBytes(leaf.Script),
		ControlBlock:      cloneBytes(leaf.ControlBlock),
		RequiredSigners: append(
			[]XOnlyPubKey(nil), plan.ScriptPath.RequiredSigners...,
		),
	}, nil
}

func newCustomAnchorSigningContext(packet *btcpsbt.Packet,
	inputIndex uint32, prevOut *wire.TxOut,
	sighashType txscript.SigHashType, sighashBytes,
	internalKeyBytes, merkleRoot []byte) *customAnchorSigningContext {

	wireOutpoint := packet.UnsignedTx.TxIn[inputIndex].PreviousOutPoint
	var outpoint Outpoint
	copy(outpoint.Txid[:], wireOutpoint.Hash[:])
	outpoint.Index = wireOutpoint.Index

	var sighash Hash
	copy(sighash[:], sighashBytes)
	var internalKey XOnlyPubKey
	copy(internalKey[:], internalKeyBytes)

	return &customAnchorSigningContext{
		inputIndex:        inputIndex,
		prevOut:           outpoint,
		prevOutValueSat:   prevOut.Value,
		prevOutScript:     cloneBytes(prevOut.PkScript),
		sighashType:       uint32(sighashType),
		sighash:           sighash,
		internalKey:       internalKey,
		taprootMerkleRoot: cloneBytes(merkleRoot),
	}
}

func customAnchorSigningRequestID(packageDigest Hash,
	mode CustomAnchorSigningMode, inputIndex uint32) Hash {

	payload := make([]byte, 0, len(packageDigest)+1+4)
	payload = append(payload, packageDigest[:]...)
	payload = append(payload, byte(mode))
	var indexBytes [4]byte
	binary.BigEndian.PutUint32(indexBytes[:], inputIndex)
	payload = append(payload, indexBytes[:]...)

	return customAnchorDigest(customAnchorSigningRequestDomain, payload)
}

func verifyTaprootKeyPathSignature(signature []byte, sighashType uint32,
	sighash Hash, prevOutScript []byte) error {

	sig, err := parseCustomAnchorTaprootSignature(signature, sighashType)
	if err != nil {
		return err
	}
	_, witnessProgram, err := txscript.ExtractWitnessProgramInfo(
		prevOutScript,
	)
	if err != nil {
		return fmt.Errorf("taproot witness program: %w", err)
	}
	outputKey, err := schnorr.ParsePubKey(witnessProgram)
	if err != nil {
		return fmt.Errorf("taproot output key: %w", err)
	}
	if !sig.Verify(sighash[:], outputKey) {
		return fmt.Errorf("taproot key-path signature verification failed")
	}

	return nil
}

func verifyTaprootScriptPathSignature(signature []byte, sighashType uint32,
	sighash Hash, signer XOnlyPubKey) error {

	sig, err := parseCustomAnchorTaprootSignature(signature, sighashType)
	if err != nil {
		return err
	}
	publicKey, err := schnorr.ParsePubKey(signer[:])
	if err != nil {
		return fmt.Errorf("script-path signer: %w", err)
	}
	if !sig.Verify(sighash[:], publicKey) {
		return fmt.Errorf("script-path signature verification failed")
	}

	return nil
}

func parseCustomAnchorTaprootSignature(signature []byte,
	sighashType uint32) (*schnorr.Signature, error) {

	if len(signature) != schnorr.SignatureSize &&
		len(signature) != schnorr.SignatureSize+1 {

		return nil, fmt.Errorf("taproot signature must be 64 or 65 bytes")
	}
	if sighashType > 0xff {
		return nil, fmt.Errorf(
			"taproot sighash type does not fit in one byte",
		)
	}
	if len(signature) == schnorr.SignatureSize &&
		sighashType != uint32(txscript.SigHashDefault) {

		return nil, fmt.Errorf(
			"non-default taproot sighash requires a suffix",
		)
	}
	if len(signature) == schnorr.SignatureSize+1 {
		if sighashType == uint32(txscript.SigHashDefault) {
			return nil, fmt.Errorf(
				"default taproot sighash must not have a suffix",
			)
		}
		if signature[schnorr.SignatureSize] != byte(sighashType) {
			return nil, fmt.Errorf(
				"taproot signature sighash suffix mismatch",
			)
		}
	}

	sig, err := schnorr.ParseSignature(signature[:schnorr.SignatureSize])
	if err != nil {
		return nil, fmt.Errorf("invalid taproot signature: %w", err)
	}

	return sig, nil
}

func validateCustomAnchorTapscript(script []byte) error {
	tokenizer := txscript.MakeScriptTokenizer(0, script)
	for tokenizer.Next() {
		if tokenizer.Opcode() == txscript.OP_CODESEPARATOR {
			return fmt.Errorf(
				"script-path OP_CODESEPARATOR is unsupported",
			)
		}
	}
	if err := tokenizer.Err(); err != nil {
		return fmt.Errorf("invalid script-path tapscript: %w", err)
	}

	return nil
}

func verifyScriptPathWitness(tx *wire.MsgTx, inputIndex int,
	prevOut *wire.TxOut, prevOutFetcher txscript.PrevOutputFetcher,
	sigHashes *txscript.TxSigHashes, witness wire.TxWitness) error {

	txWithWitness := tx.Copy()
	txWithWitness.TxIn[inputIndex].Witness = witness
	vm, err := txscript.NewEngine(
		prevOut.PkScript, txWithWitness, inputIndex,
		txscript.StandardVerifyFlags, nil, sigHashes, prevOut.Value,
		prevOutFetcher,
	)
	if err != nil {
		return fmt.Errorf("script-path witness engine: %w", err)
	}
	if err := vm.Execute(); err != nil {
		return fmt.Errorf("script-path witness verification failed: %w", err)
	}

	return nil
}

func validateMuSig2Aggregate(internalKey XOnlyPubKey,
	participants []PubKey) error {

	keys := make([]*btcec.PublicKey, len(participants))
	for i, participant := range participants {
		key, err := btcec.ParsePubKey(participant[:])
		if err != nil {
			return fmt.Errorf("participant %d: %w", i, err)
		}
		keys[i] = key
	}

	aggregate, _, _, err := musig2.AggregateKeys(keys, false)
	if err != nil {
		return fmt.Errorf("aggregate participant keys: %w", err)
	}
	if !bytes.Equal(
		schnorr.SerializePubKey(aggregate.PreTweakedKey), internalKey[:],
	) {

		return fmt.Errorf("participants do not match the committed internal key")
	}

	return nil
}

func validateCustomAnchorXOnlyKeys(label string, keys []XOnlyPubKey) error {
	seen := make(map[XOnlyPubKey]struct{}, len(keys))
	for i, key := range keys {
		if err := validateCustomAnchorXOnlyKey(label, key); err != nil {
			return fmt.Errorf("%s %d: %w", label, i, err)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate %s %d", label, i)
		}
		seen[key] = struct{}{}
	}

	return nil
}

func validateCustomAnchorXOnlyKey(label string, key XOnlyPubKey) error {
	_, err := schnorr.ParsePubKey(key[:])
	if err != nil {
		return fmt.Errorf("invalid %s: %w", label, err)
	}

	return nil
}

func validateCustomAnchorPubKeys(label string, keys []PubKey) error {
	seen := make(map[PubKey]struct{}, len(keys))
	for i, key := range keys {
		if _, err := btcec.ParsePubKey(key[:]); err != nil {
			return fmt.Errorf("%s %d: invalid %s: %w", label, i,
				label, err)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate %s %d", label, i)
		}
		seen[key] = struct{}{}
	}

	return nil
}

func anchorPrevOuts(packet *btcpsbt.Packet) (
	*txscript.MultiPrevOutFetcher, []*wire.TxOut, error) {

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	prevOuts := make([]*wire.TxOut, len(packet.Inputs))
	seen := make(map[wire.OutPoint]struct{}, len(packet.Inputs))
	for i := range packet.Inputs {
		wireOutpoint := packet.UnsignedTx.TxIn[i].PreviousOutPoint
		if _, ok := seen[wireOutpoint]; ok {
			return nil, nil, fmt.Errorf(
				"anchor input %d repeats a previous outpoint", i,
			)
		}
		seen[wireOutpoint] = struct{}{}

		prevOut, err := anchorPrevOut(packet, i)
		if err != nil {
			return nil, nil, err
		}
		prevOuts[i] = prevOut
		prevOutFetcher.AddPrevOut(wireOutpoint, prevOut)
	}

	return prevOutFetcher, prevOuts, nil
}

func anchorPrevOut(packet *btcpsbt.Packet, inputIndex int) (
	*wire.TxOut, error) {

	pInput := &packet.Inputs[inputIndex]
	wireOutpoint := packet.UnsignedTx.TxIn[inputIndex].PreviousOutPoint
	var nonWitnessOut *wire.TxOut
	if pInput.NonWitnessUtxo != nil {
		if pInput.NonWitnessUtxo.TxHash() != wireOutpoint.Hash {
			return nil, fmt.Errorf(
				"input %d non-witness UTXO transaction mismatch",
				inputIndex,
			)
		}
		if wireOutpoint.Index >= uint32(len(pInput.NonWitnessUtxo.TxOut)) {
			return nil, fmt.Errorf(
				"input %d non-witness UTXO index is out of range",
				inputIndex,
			)
		}
		nonWitnessOut = pInput.NonWitnessUtxo.TxOut[wireOutpoint.Index]
	}

	if pInput.WitnessUtxo != nil {
		if nonWitnessOut != nil &&
			!equalWireTxOut(nonWitnessOut, pInput.WitnessUtxo) {

			return nil, fmt.Errorf(
				"input %d witness and non-witness UTXOs differ",
				inputIndex,
			)
		}

		return &wire.TxOut{
			Value:    pInput.WitnessUtxo.Value,
			PkScript: cloneBytes(pInput.WitnessUtxo.PkScript),
		}, nil
	}
	if nonWitnessOut != nil {
		return &wire.TxOut{
			Value:    nonWitnessOut.Value,
			PkScript: cloneBytes(nonWitnessOut.PkScript),
		}, nil
	}

	return nil, fmt.Errorf("input %d previous output is required", inputIndex)
}

func equalWireTxOut(a, b *wire.TxOut) bool {
	return a.Value == b.Value && bytes.Equal(a.PkScript, b.PkScript)
}

func decodeAnchorPSBT(raw []byte) (*btcpsbt.Packet, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty PSBT")
	}

	packet, err := btcpsbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		return nil, err
	}
	if err := packet.SanityCheck(); err != nil {
		return nil, err
	}

	return packet, nil
}

func serializeAnchorPSBT(packet *btcpsbt.Packet) ([]byte, error) {
	var buffer bytes.Buffer
	if err := packet.Serialize(&buffer); err != nil {
		return nil, fmt.Errorf("serialize anchor PSBT: %w", err)
	}

	return buffer.Bytes(), nil
}

func canonicalAnchorPSBT(raw []byte, stripSignatures bool) ([]byte, error) {
	packet, err := decodeAnchorPSBT(raw)
	if err != nil {
		return nil, err
	}
	if stripSignatures {
		for i := range packet.Inputs {
			packet.Inputs[i].PartialSigs = nil
			packet.Inputs[i].FinalScriptSig = nil
			packet.Inputs[i].FinalScriptWitness = nil
			packet.Inputs[i].TaprootKeySpendSig = nil
			packet.Inputs[i].TaprootScriptSpendSig = nil
		}
	}

	return serializeAnchorPSBT(packet)
}

func customAnchorUnsignedTxDigest(raw []byte) (Hash, error) {
	packet, err := decodeAnchorPSBT(raw)
	if err != nil {
		return Hash{}, fmt.Errorf("anchor PSBT: %w", err)
	}

	var txBytes bytes.Buffer
	if err := packet.UnsignedTx.SerializeNoWitness(&txBytes); err != nil {
		return Hash{}, fmt.Errorf("serialize unsigned anchor transaction: %w", err)
	}

	return customAnchorDigest(
		customAnchorUnsignedTxDigestDomain, txBytes.Bytes(),
	), nil
}

func compareUnsignedAnchorTransactions(committed, final *wire.MsgTx) error {
	if committed.Version != final.Version {
		return fmt.Errorf("anchor transaction version changed")
	}
	if committed.LockTime != final.LockTime {
		return fmt.Errorf("anchor transaction locktime changed")
	}
	if len(committed.TxIn) != len(final.TxIn) {
		return fmt.Errorf("anchor transaction input count changed")
	}
	for i := range committed.TxIn {
		committedInput := committed.TxIn[i]
		finalInput := final.TxIn[i]
		if committedInput.PreviousOutPoint != finalInput.PreviousOutPoint {
			return fmt.Errorf("anchor input %d previous outpoint changed", i)
		}
		if committedInput.Sequence != finalInput.Sequence {
			return fmt.Errorf("anchor input %d sequence changed", i)
		}
		if !bytes.Equal(
			committedInput.SignatureScript, finalInput.SignatureScript,
		) {

			return fmt.Errorf("anchor input %d unsigned script changed", i)
		}
		if len(finalInput.Witness) != len(committedInput.Witness) {
			return fmt.Errorf("anchor input %d unsigned witness changed", i)
		}
		for j := range committedInput.Witness {
			if !bytes.Equal(
				committedInput.Witness[j], finalInput.Witness[j],
			) {

				return fmt.Errorf(
					"anchor input %d unsigned witness changed", i,
				)
			}
		}
	}
	if len(committed.TxOut) != len(final.TxOut) {
		return fmt.Errorf("anchor transaction output count changed")
	}
	for i := range committed.TxOut {
		committedOutput := committed.TxOut[i]
		finalOutput := final.TxOut[i]
		if committedOutput.Value != finalOutput.Value {
			return fmt.Errorf("anchor output %d value changed", i)
		}
		if !bytes.Equal(committedOutput.PkScript, finalOutput.PkScript) {
			return fmt.Errorf("anchor output %d script changed", i)
		}
	}

	return nil
}
