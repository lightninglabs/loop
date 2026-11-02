package tapsdk

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"sort"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/mempool"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightninglabs/taproot-assets/vm"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	customAnchorCheckRequest       = "request_valid"
	customAnchorCheckProofChain    = "proof_chain_valid"
	customAnchorCheckBTCPath       = "unconfirmed_anchor_valid"
	customAnchorCheckAssetPath     = "asset_proof_path_valid"
	customAnchorCheckProofIdentity = "proof_identity_valid"
	customAnchorCheckAnchorInput   = "anchor_input_commitment_valid"
	customAnchorCheckVPackets      = "virtual_packets_valid"
)

var canonicalP2AScript = []byte{0x51, 0x02, 0x4e, 0x73}

// CustomAnchorTxBuilder builds inspectable Taproot Asset state transitions
// around a caller-controlled Bitcoin anchor PSBT. It is intentionally separate
// from TxBuilder: callers that don't need exact anchor control should continue
// to use the simpler send APIs.
type CustomAnchorTxBuilder struct {
	wallet                 *Wallet
	params                 *address.ChainParams
	confirmedProofVerifier ConfirmedProofVerifier
}

// SetConfirmedProofVerifier sets the chain and inventory verifier required by
// compact proof-path inputs. For a non-empty path, the verifier must also
// implement UnconfirmedAnchorVerifier. Confirmed proof-file inputs continue to
// use the configured wallet proof client.
func (b *CustomAnchorTxBuilder) SetConfirmedProofVerifier(
	verifier ConfirmedProofVerifier) *CustomAnchorTxBuilder {

	if b != nil {
		b.confirmedProofVerifier = verifier
	}

	return b
}

// CustomAnchorPlan is an immutable, pre-commit custom-anchor snapshot. Its
// byte slices and request are only exposed through cloning accessors.
type CustomAnchorPlan struct {
	client Client

	request             *CustomAnchorRequest
	anchorPSBT          []byte
	activeVirtualPSBTs  [][]byte
	passiveVirtualPSBTs [][]byte
	backendSigning      []bool
	inputs              []CustomAnchorAssetInputSummary
	outputs             []customAnchorPlannedOutput
	verification        CustomAnchorVerificationResult
}

type customAnchorInputMaterial struct {
	requestIndex uint32
	request      CustomAssetInput
	proof        *proof.Proof
	assetRef     AssetRef
	issuanceID   AssetID
	assetType    AssetType
}

type customAnchorOutputMaterial struct {
	requestIndex uint32
	request      CustomAssetOutput
	internalKey  *btcec.PublicKey
	sibling      *commitment.TapscriptPreimage
	scriptKey    tapsend.ScriptKeyGen
	proofURL     *url.URL
}

type customAnchorPassiveMaterial struct {
	requestIndex uint32
	request      CustomAnchorPassivePacket
	packet       *tappsbt.VPacket
	proof        *proof.Proof
	assetRef     AssetRef
	issuanceID   AssetID
	assetType    AssetType
}

type customAnchorPlannedOutput struct {
	LogicalOutputID string
	RequestIndex    uint32
	PacketRole      CustomAnchorPacketRole
	PacketIndex     uint32
	VirtualOutput   uint32
	AnchorOutput    uint32
	AssetRef        AssetRef
	IssuanceID      AssetID
	AssetType       AssetType
	Amount          uint64
	AnchorValueSat  uint64
	ScriptKey       PubKey
	ScriptMode      CustomAssetScriptMode
	ProofDelivery   CustomAssetProofDelivery
	OPTrueSpend     *CustomAssetOPTrueSpendInfo
}

// CustomAnchorPlannedOutputSummary describes one inspected asset output
// before the backend commits its proof suffix and final anchor outpoint.
type CustomAnchorPlannedOutputSummary struct {
	// LogicalOutputID is the stable caller-defined output identifier.
	LogicalOutputID string

	// LogicalOutputIndex is the output position in the build request.
	LogicalOutputIndex uint32

	// PacketIndex identifies the prepared active virtual packet.
	PacketIndex uint32

	// PacketRole identifies the active packet collection.
	PacketRole CustomAnchorPacketRole

	// VirtualOutputIndex identifies the output within PacketIndex.
	VirtualOutputIndex uint32

	// AnchorOutputIndex identifies the output in the anchor transaction.
	AnchorOutputIndex uint32

	// AssetRef is the caller-facing semantic asset identity.
	AssetRef AssetRef

	// IssuanceID is the concrete issuance created by this packet output.
	IssuanceID AssetID

	// AssetType is the concrete asset type for the output.
	AssetType AssetType

	// Amount is the number of asset units assigned to the output.
	Amount uint64

	// AnchorValueSat is the BTC value assigned to the anchor output.
	AnchorValueSat uint64

	// ScriptKey is the concrete asset script key for the output.
	ScriptKey PubKey

	// ScriptMode records how the concrete script key was constructed.
	ScriptMode CustomAssetScriptMode

	// ProofDelivery preserves the complete host-owned delivery metadata.
	ProofDelivery CustomAssetProofDelivery

	// OPTrueSpend contains the exact script-path witness data when the output
	// uses CustomAssetScriptOPTrue. Other script modes leave it nil.
	OPTrueSpend *CustomAssetOPTrueSpendInfo
}

// CustomAnchorOutputCommitmentPreview describes the Taproot commitment roots
// a custom-anchor plan will insert into one Bitcoin output. The preview is
// computed locally and does not sign or commit a virtual transaction or mutate
// the plan.
type CustomAnchorOutputCommitmentPreview struct {
	// LogicalOutputID is the stable caller-defined output allocation ID.
	LogicalOutputID string

	// LogicalOutputIndex is the output position in the build request.
	LogicalOutputIndex uint32

	// PacketIndex identifies the prepared virtual packet.
	PacketIndex uint32

	// PacketRole identifies the active or passive packet collection.
	PacketRole CustomAnchorPacketRole

	// VirtualOutputIndex identifies the output within PacketIndex.
	VirtualOutputIndex uint32

	// AnchorOutputIndex identifies the output in the anchor transaction.
	AnchorOutputIndex uint32

	// TaprootAssetRoot is the root of the Taproot Asset commitment before
	// combining it with the host-supplied tapscript sibling.
	TaprootAssetRoot Hash

	// TaprootMerkleRoot is the final BIP341 root that combines the Taproot
	// Asset commitment with the optional host-supplied tapscript sibling.
	TaprootMerkleRoot Hash
}

// NewCustomAnchorTxBuilder creates the advanced proof-selected builder.
func (s *Wallet) NewCustomAnchorTxBuilder() *CustomAnchorTxBuilder {
	params, err := address.Net(s.networkHRP)
	if err != nil {
		panic(fmt.Sprintf("invalid wallet network HRP %q: %v",
			s.networkHRP, err))
	}

	return &CustomAnchorTxBuilder{
		wallet: s,
		params: params,
	}
}

// Request returns a deep copy of the request bound to this plan.
func (p *CustomAnchorPlan) Request() *CustomAnchorRequest {
	if p == nil || p.request == nil {
		return nil
	}

	return p.request.Clone()
}

// AnchorPSBT returns the locally verified, unsigned anchor PSBT template.
func (p *CustomAnchorPlan) AnchorPSBT() []byte {
	if p == nil {
		return nil
	}

	return cloneBytes(p.anchorPSBT)
}

// ActiveVirtualPSBTs returns the prepared active virtual packets.
func (p *CustomAnchorPlan) ActiveVirtualPSBTs() [][]byte {
	if p == nil {
		return nil
	}

	return cloneByteSlices(p.activeVirtualPSBTs)
}

// PassiveVirtualPSBTs returns the caller-supplied passive packets.
func (p *CustomAnchorPlan) PassiveVirtualPSBTs() [][]byte {
	if p == nil {
		return nil
	}

	return cloneByteSlices(p.passiveVirtualPSBTs)
}

// Inputs returns a deep copy of the exact proof-selected active inputs and
// their logical-to-virtual mappings.
func (p *CustomAnchorPlan) Inputs() []CustomAnchorAssetInputSummary {
	if p == nil {
		return nil
	}

	return cloneCustomAnchorInputSummaries(p.inputs)
}

// Outputs returns a deep copy of the concrete active output mappings inspected
// by the builder.
func (p *CustomAnchorPlan) Outputs() []CustomAnchorPlannedOutputSummary {
	if p == nil {
		return nil
	}

	result := make(
		[]CustomAnchorPlannedOutputSummary, len(p.outputs),
	)
	for idx := range p.outputs {
		output := p.outputs[idx]
		result[idx] = CustomAnchorPlannedOutputSummary{
			LogicalOutputID:    output.LogicalOutputID,
			LogicalOutputIndex: output.RequestIndex,
			PacketIndex:        output.PacketIndex,
			PacketRole:         output.PacketRole,
			VirtualOutputIndex: output.VirtualOutput,
			AnchorOutputIndex:  output.AnchorOutput,
			AssetRef:           output.AssetRef,
			IssuanceID:         output.IssuanceID,
			AssetType:          output.AssetType,
			Amount:             output.Amount,
			AnchorValueSat:     output.AnchorValueSat,
			ScriptKey:          output.ScriptKey,
			ScriptMode:         output.ScriptMode,
			ProofDelivery: CustomAssetProofDelivery{
				RecipientID:    output.ProofDelivery.RecipientID,
				CourierAddress: output.ProofDelivery.CourierAddress,
				OpaqueMetadata: cloneBytes(
					output.ProofDelivery.OpaqueMetadata,
				),
			},
			OPTrueSpend: output.OPTrueSpend.Clone(),
		}
	}

	return result
}

// PreviewOutputCommitments derives the exact output commitment roots without
// signing virtual inputs or calling CommitVirtualPsbts. It is intended for
// hosts that must compose and canonically order their final Bitcoin outputs
// before committing the transition.
//
// Taproot Asset V1 output commitment leaves use witness-stripped asset
// encoding, so the roots are independent of a later backend virtual-input
// signature. Split commitments bind the anchor output index, however, so
// callers that reindex outputs after a preview must rebuild and preview again
// before committing.
func (p *CustomAnchorPlan) PreviewOutputCommitments() (
	[]CustomAnchorOutputCommitmentPreview, error) {

	if p == nil || p.client == nil || p.request == nil {
		return nil, fmt.Errorf("nil custom anchor plan")
	}
	if !p.verification.Valid() {
		return nil, fmt.Errorf("custom anchor plan verification is not " +
			"valid")
	}
	if len(p.activeVirtualPSBTs) != len(p.backendSigning) {
		return nil, fmt.Errorf("custom anchor plan signing classification " +
			"is incomplete")
	}
	active, err := decodeVirtualPackets(
		"preview active", p.activeVirtualPSBTs,
	)
	if err != nil {
		return nil, err
	}
	passive, err := decodeVirtualPackets(
		"preview passive", p.passiveVirtualPSBTs,
	)
	if err != nil {
		return nil, err
	}
	allPackets := append(
		append([]*tappsbt.VPacket(nil), active...), passive...,
	)
	commitments, err := tapsend.CreateOutputCommitments(
		allPackets, tapsend.WithSpenderLeaves(),
	)
	if err != nil {
		return nil, fmt.Errorf("preview output commitments: %w", err)
	}

	previews := make(
		[]CustomAnchorOutputCommitmentPreview, 0, len(p.outputs),
	)
	for idx := range p.outputs {
		planned := p.outputs[idx]
		packets := active
		if planned.PacketRole == CustomAnchorPacketRolePassive {
			packets = passive
		} else if planned.PacketRole != CustomAnchorPacketRoleActive {
			return nil, fmt.Errorf("planned output %q has unknown packet "+
				"role %d", planned.LogicalOutputID,
				planned.PacketRole)
		}
		if planned.PacketIndex >= uint32(len(packets)) {
			return nil, fmt.Errorf("planned output %q packet index is "+
				"out of range", planned.LogicalOutputID)
		}
		packet := packets[planned.PacketIndex]
		if planned.VirtualOutput >= uint32(len(packet.Outputs)) {
			return nil, fmt.Errorf("planned output %q virtual index is "+
				"out of range", planned.LogicalOutputID)
		}
		virtualOutput := packet.Outputs[planned.VirtualOutput]
		assetRoot, merkleRoot, err := deriveCustomAnchorOutputRoots(
			virtualOutput, commitments,
		)
		if err != nil {
			return nil, fmt.Errorf("preview output %q commitment roots: "+
				"%w", planned.LogicalOutputID, err)
		}

		previews = append(previews,
			CustomAnchorOutputCommitmentPreview{
				LogicalOutputID:    planned.LogicalOutputID,
				LogicalOutputIndex: planned.RequestIndex,
				PacketIndex:        planned.PacketIndex,
				PacketRole:         planned.PacketRole,
				VirtualOutputIndex: planned.VirtualOutput,
				AnchorOutputIndex:  planned.AnchorOutput,
				TaprootAssetRoot:   assetRoot,
				TaprootMerkleRoot:  merkleRoot,
			},
		)
	}

	return previews, nil
}

// Verification returns a copy of the structured pre-commit verification
// report.
func (p *CustomAnchorPlan) Verification() CustomAnchorVerificationResult {
	if p == nil {
		return CustomAnchorVerificationResult{}
	}

	return cloneCustomAnchorVerification(p.verification)
}

// Build validates proof-selected inputs, constructs one V1 virtual packet per
// concrete issuance, and merges the required Taproot Asset PSBT metadata into
// the caller's exact anchor transaction. No backend or anchor signature is
// requested by this method.
func (b *CustomAnchorTxBuilder) Build(ctx context.Context,
	req *CustomAnchorRequest) (*CustomAnchorPlan, error) {

	if b == nil || b.wallet == nil || b.wallet.client == nil {
		return nil, fmt.Errorf("custom anchor builder has no wallet client")
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("validate custom anchor request: %w", err)
	}

	request := req.Clone()
	verification := CustomAnchorVerificationResult{}
	addCustomAnchorCheck(
		&verification, customAnchorCheckRequest,
		CustomAnchorVerificationScopeRequest,
		CustomAnchorVerificationOriginLocal, nil, "",
		"request structure and amount conservation are valid",
	)

	inputs, err := b.resolveInputs(ctx, request, &verification)
	if err != nil {
		return nil, err
	}

	outputs, err := b.resolveOutputs(ctx, request, inputs)
	if err != nil {
		return nil, err
	}

	activePackets, plannedOutputs, backendSigning, err := b.buildActivePackets(
		ctx, inputs, outputs,
	)
	if err != nil {
		return nil, err
	}

	passiveMaterials, passivePackets, passiveBytes, err :=
		b.resolvePassivePackets(
			ctx, request.PassiveAssets, activePackets, &verification,
		)
	if err != nil {
		return nil, err
	}

	allPackets := append(
		append([]*tappsbt.VPacket(nil), activePackets...),
		passivePackets...,
	)
	if err := tapsend.ValidateCommitmentKeysUnique(allPackets); err != nil {
		return nil, fmt.Errorf("validate virtual packet commitment keys: %w",
			err)
	}

	anchorPacket, err := prepareCustomAnchorPSBT(
		request, activePackets, passivePackets,
	)
	if err != nil {
		return nil, err
	}
	if _, err := customAnchorSigningRequests(
		anchorPacket, request.SigningPlans, nil, Hash{},
	); err != nil {
		return nil, fmt.Errorf("validate anchor signing plans: %w", err)
	}
	if err := validateCustomAnchorBitcoinTransaction(
		anchorPacket,
		request.Funding.Mode != CustomAnchorFundingWalletFunded,
	); err != nil {
		return nil, fmt.Errorf("validate anchor transaction: %w", err)
	}
	if err := tapsend.ValidateAnchorInputs(
		anchorPacket, allPackets, nil,
	); err != nil {
		return nil, fmt.Errorf("validate complete anchor input commitment: %w",
			err)
	}
	addCustomAnchorCheck(
		&verification, customAnchorCheckAnchorInput,
		CustomAnchorVerificationScopeInputProof,
		CustomAnchorVerificationOriginLocal, nil, "",
		"active and passive packets exactly reconstruct every asset input",
	)

	anchorBytes, err := serializePSBT(anchorPacket)
	if err != nil {
		return nil, fmt.Errorf("serialize prepared anchor PSBT: %w", err)
	}
	activeBytes, err := encodeVirtualPackets(activePackets)
	if err != nil {
		return nil, err
	}
	addCustomAnchorCheck(
		&verification, customAnchorCheckVPackets,
		CustomAnchorVerificationScopeOutputCommitment,
		CustomAnchorVerificationOriginLocal, nil, "",
		"V1 virtual packets are prepared with unique commitment keys",
	)

	inputSummaries, err := mapCustomAnchorInputs(
		anchorPacket, inputs, activePackets,
	)
	if err != nil {
		return nil, err
	}
	passiveInputs, passiveOutputs, err := mapCustomAnchorPassivePackets(
		anchorPacket, passiveMaterials,
	)
	if err != nil {
		return nil, err
	}
	inputSummaries = append(inputSummaries, passiveInputs...)
	plannedOutputs = append(plannedOutputs, passiveOutputs...)

	return &CustomAnchorPlan{
		client:              b.wallet.client,
		request:             request,
		anchorPSBT:          anchorBytes,
		activeVirtualPSBTs:  activeBytes,
		passiveVirtualPSBTs: passiveBytes,
		backendSigning:      append([]bool(nil), backendSigning...),
		inputs:              inputSummaries,
		outputs:             plannedOutputs,
		verification:        verification,
	}, nil
}

func (b *CustomAnchorTxBuilder) resolveInputs(ctx context.Context,
	req *CustomAnchorRequest,
	verification *CustomAnchorVerificationResult) (
	[]customAnchorInputMaterial, error) {

	inputs := make([]customAnchorInputMaterial, len(req.Inputs))
	seenPrevIDs := make(map[asset.PrevID]struct{}, len(req.Inputs))
	for idx := range req.Inputs {
		requested := req.Inputs[idx]
		var (
			lastProof        *proof.Proof
			pathSummary      *AssetProofPathSummary
			verificationFrom = CustomAnchorVerificationOriginBackend
			verificationText = "backend verified the confirmed proof chain"
		)
		if requested.ProofPath != nil {
			if b.confirmedProofVerifier == nil {
				return nil, fmt.Errorf("input %d proof path: confirmed proof "+
					"verifier is required", idx)
			}
			var err error
			pathSummary, err = requested.ProofPath.Verify(
				ctx, b.confirmedProofVerifier,
			)
			if err != nil {
				return nil, fmt.Errorf("verify input %d proof path: %w", idx,
					err)
			}
			if len(requested.ProofPath.Steps) == 0 {
				lastProof, err = decodeAssetProofPathBase(
					requested.ProofPath.ConfirmedBaseProof,
				)
			} else {
				tip := len(requested.ProofPath.Steps) - 1
				lastProof, err = decodeAssetProofPathStep(
					&requested.ProofPath.Steps[tip],
					requested.ProofPath.stepWitnessCount(tip),
				)
			}
			if err != nil {
				return nil, fmt.Errorf("decode input %d proof path tip: %w",
					idx, err)
			}
			verificationFrom = CustomAnchorVerificationOriginHost
			verificationText = "host verified the confirmed proof base"
		} else {
			file, err := proof.DecodeFile(requested.ProofFile)
			if err != nil {
				return nil, fmt.Errorf("decode input %d proof file: %w", idx,
					err)
			}
			lastProof, err = file.LastProof()
			if err != nil {
				return nil, fmt.Errorf("read input %d last proof: %w", idx,
					err)
			}
		}
		if lastProof.Asset.LockTime != 0 ||
			lastProof.Asset.RelativeLockTime != 0 {

			return nil, fmt.Errorf("input %d asset timelocks are not "+
				"supported", idx)
		}

		actualRef, issuanceID, assetType, err := assetIdentity(
			&lastProof.Asset,
		)
		if err != nil {
			return nil, fmt.Errorf("resolve input %d asset identity: %w",
				idx, err)
		}
		if !customAnchorAssetRefMatchesIssuance(
			requested.AssetRef, actualRef, issuanceID,
		) {

			source := "proof"
			if requested.ProofPath != nil {
				source = "path"
			}
			return nil, fmt.Errorf("input %d %s asset ref %q does not "+
				"match requested %q", idx, source, actualRef,
				requested.AssetRef)
		}
		if requested.Amount != lastProof.Asset.Amount {
			source := "proof"
			if requested.ProofPath != nil {
				source = "path"
			}
			return nil, fmt.Errorf("input %d %s amount %d does not "+
				"match requested %d", idx, source, lastProof.Asset.Amount,
				requested.Amount)
		}

		if pathSummary != nil {
			proofScriptKey, err := ParsePubKey(
				lastProof.Asset.ScriptKey.PubKey.SerializeCompressed(),
			)
			if err != nil {
				return nil, err
			}
			if !pathSummary.AssetRef.Equivalent(actualRef) ||
				pathSummary.IssuanceID != issuanceID ||
				pathSummary.AssetType != assetType ||
				pathSummary.Amount != lastProof.Asset.Amount ||
				pathSummary.ScriptKey != proofScriptKey ||
				pathSummary.AnchorOutpoint != outpointFromWire(
					lastProof.OutPoint()) {

				return nil, fmt.Errorf("input %d proof path summary does not "+
					"match its verified tip", idx)
			}
		} else {
			verified, err := b.wallet.client.VerifyProof(
				ctx, requested.ProofFile,
			)
			if err != nil {
				return nil, fmt.Errorf("verify input %d proof chain: %w", idx,
					err)
			}
			if verified == nil || !verified.Valid ||
				verified.DecodedProof == nil {

				return nil, fmt.Errorf("input %d proof chain is not valid", idx)
			}
			backendIdentityMatches := requested.AssetRef.IsAssetIDRef() ||
				verified.DecodedProof.AssetRef.Equivalent(requested.AssetRef)
			proofScriptKey, err := ParsePubKey(
				lastProof.Asset.ScriptKey.PubKey.SerializeCompressed(),
			)
			if err != nil {
				return nil, err
			}
			if !backendIdentityMatches ||
				verified.DecodedProof.IssuanceID != issuanceID ||
				verified.DecodedProof.Amount != requested.Amount ||
				verified.DecodedProof.ScriptKey != proofScriptKey ||
				verified.DecodedProof.Outpoint != outpointFromWire(
					lastProof.OutPoint()) {

				return nil, fmt.Errorf("input %d backend proof summary does "+
					"not match the locally decoded proof", idx)
			}
		}

		inputIndex := uint32(idx)
		addCustomAnchorCheck(
			verification, customAnchorCheckProofChain,
			CustomAnchorVerificationScopeInputProof,
			verificationFrom, &inputIndex, "", verificationText,
		)
		if pathSummary != nil {
			if len(requested.ProofPath.Steps) != 0 {
				addCustomAnchorCheck(
					verification, customAnchorCheckBTCPath,
					CustomAnchorVerificationScopeInputProof,
					CustomAnchorVerificationOriginHost,
					&inputIndex, "", "host attested every "+
						"unconfirmed Bitcoin anchor",
				)
			}
			pathMessage := "SDK verified the compact Taproot " +
				"Assets path"
			coPathCount, coPathDepth := requested.ProofPath.
				coInputStats()
			if coPathCount > 0 {
				pathMessage = fmt.Sprintf(
					"%s (%d co-input paths, max depth %d)",
					pathMessage, coPathCount, coPathDepth,
				)
			}
			addCustomAnchorCheck(
				verification, customAnchorCheckAssetPath,
				CustomAnchorVerificationScopeInputProof,
				CustomAnchorVerificationOriginLocal, &inputIndex, "",
				pathMessage,
			)
		}
		addCustomAnchorCheck(
			verification, customAnchorCheckProofIdentity,
			CustomAnchorVerificationScopeAssetIdentity,
			CustomAnchorVerificationOriginLocal, &inputIndex, "",
			"proof identity and exact amount match the request",
		)

		input := customAnchorInputMaterial{
			requestIndex: uint32(idx),
			request:      requested,
			proof:        lastProof,
			assetRef:     actualRef,
			issuanceID:   issuanceID,
			assetType:    assetType,
		}
		prevID := customAnchorInputPrevID(input)
		if _, ok := seenPrevIDs[prevID]; ok {
			return nil, fmt.Errorf("input %d duplicates asset predecessor %s",
				idx, prevID)
		}
		seenPrevIDs[prevID] = struct{}{}
		inputs[idx] = input
	}

	return inputs, nil
}

func (b *CustomAnchorTxBuilder) resolveOutputs(ctx context.Context,
	req *CustomAnchorRequest, inputs []customAnchorInputMaterial) (
	[]customAnchorOutputMaterial, error) {

	outputs := make([]customAnchorOutputMaterial, len(req.Outputs))
	seen := make(map[string]struct{}, len(req.Outputs))
	issuances := make(map[string]map[AssetID]struct{})
	for idx := range inputs {
		key := customAnchorAssetKey(inputs[idx].request.AssetRef)
		if issuances[key] == nil {
			issuances[key] = make(map[AssetID]struct{})
		}
		issuances[key][inputs[idx].issuanceID] = struct{}{}
	}
	for idx := range req.Outputs {
		requested := req.Outputs[idx]
		if requested.AnchorValueSat > math.MaxInt64 {
			return nil, fmt.Errorf("output %d anchor value exceeds int64",
				idx)
		}
		identity := fmt.Sprintf("%s:%d", customAnchorAssetKey(
			requested.AssetRef), requested.AnchorOutputIndex)
		if _, ok := seen[identity]; ok {
			return nil, fmt.Errorf("output %d duplicates asset ref and "+
				"anchor output index", idx)
		}
		seen[identity] = struct{}{}

		internalKey, err := btcec.ParsePubKey(
			requested.Anchor.InternalKey.PubKey[:],
		)
		if err != nil {
			return nil, fmt.Errorf("parse output %d anchor internal key: %w",
				idx, err)
		}
		sibling, err := customAnchorSibling(requested.Anchor.Tapscript)
		if err != nil {
			return nil, fmt.Errorf("build output %d anchor sibling: %w",
				idx, err)
		}
		multiIssuance := len(issuances[customAnchorAssetKey(requested.AssetRef)]) > 1
		scriptKey, err := b.customAssetScriptKey(
			ctx, requested.Script, multiIssuance,
		)
		if err != nil {
			return nil, fmt.Errorf("build output %d asset script: %w", idx,
				err)
		}

		var proofURL *url.URL
		if requested.ProofDelivery.CourierAddress != "" {
			proofURL, err = url.ParseRequestURI(
				requested.ProofDelivery.CourierAddress,
			)
			if err != nil {
				return nil, fmt.Errorf("parse output %d proof courier: %w",
					idx, err)
			}
		}

		outputs[idx] = customAnchorOutputMaterial{
			requestIndex: uint32(idx),
			request:      requested,
			internalKey:  internalKey,
			sibling:      sibling,
			scriptKey:    scriptKey,
			proofURL:     proofURL,
		}
	}

	return outputs, nil
}

func (b *CustomAnchorTxBuilder) buildActivePackets(ctx context.Context,
	inputs []customAnchorInputMaterial, outputs []customAnchorOutputMaterial) (
	[]*tappsbt.VPacket,
	[]customAnchorPlannedOutput, []bool, error) {

	inputGroups := make(map[string][]customAnchorInputMaterial)
	for idx := range inputs {
		key := customAnchorAssetKey(inputs[idx].request.AssetRef)
		inputGroups[key] = append(inputGroups[key], inputs[idx])
	}

	groupKeys := make([]string, 0, len(inputGroups))
	for key := range inputGroups {
		groupKeys = append(groupKeys, key)
	}
	sort.Strings(groupKeys)

	var (
		packets        []*tappsbt.VPacket
		backendSigning []bool
	)
	for _, key := range groupKeys {
		groupInputs := inputGroups[key]
		groupProofs := make([]*proof.Proof, len(groupInputs))
		for idx := range groupInputs {
			groupProofs[idx] = groupInputs[idx].proof
		}

		var allocations []*tapsend.Allocation
		for idx := range outputs {
			output := outputs[idx]
			if customAnchorAssetKey(output.request.AssetRef) != key {
				continue
			}

			bip32, taprootBip32 := customAnchorKeyDerivations(
				output.request.Anchor.InternalKey, b.wallet.coinType,
			)
			allocations = append(allocations, &tapsend.Allocation{
				Type:                   tapsend.CommitAllocationToRemote,
				OutputIndex:            output.request.AnchorOutputIndex,
				InternalKey:            output.internalKey,
				Bip32Derivation:        bip32,
				TaprootBip32Derivation: taprootBip32,
				SiblingPreimage:        output.sibling,
				GenScriptKey:           output.scriptKey,
				Amount:                 output.request.Amount,
				AssetVersion:           asset.V1,
				BtcAmount: btcutil.Amount(
					output.request.AnchorValueSat,
				),
				ProofDeliveryAddress: output.proofURL,
			})
		}
		if len(allocations) == 0 {
			return nil, nil, nil, fmt.Errorf("asset input group %q has no "+
				"matching outputs", groupInputs[0].request.AssetRef)
		}

		groupPackets, err := tapsend.DistributeCoins(
			groupProofs, allocations, b.params, true, tappsbt.V1,
		)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("distribute asset group %q: %w",
				groupInputs[0].request.AssetRef, err)
		}
		burnOutputs := make(map[uint32]struct{})
		for idx := range outputs {
			output := outputs[idx]
			if customAnchorAssetKey(output.request.AssetRef) == key &&
				output.request.Script.Mode == CustomAssetScriptBurn {

				burnOutputs[output.request.AnchorOutputIndex] = struct{}{}
			}
		}
		for packetIdx := range groupPackets {
			packet := groupPackets[packetIdx]
			if len(packet.Inputs) == 0 {
				return nil, nil, nil, fmt.Errorf("virtual packet has no inputs")
			}
			for outputIdx := range packet.Outputs {
				virtualOutput := packet.Outputs[outputIdx]
				if _, ok := burnOutputs[virtualOutput.AnchorOutputIndex]; !ok {
					continue
				}

				virtualOutput.ScriptKey = asset.NewScriptKey(
					asset.DeriveBurnKey(packet.Inputs[0].PrevID),
				)
			}
			if err := tapsend.PrepareOutputAssets(ctx, packet); err != nil {
				return nil, nil, nil, fmt.Errorf("prepare virtual packet output "+
					"assets: %w", err)
			}
			if err := applyCustomAssetWitnesses(packet, groupInputs); err != nil {
				return nil, nil, nil, err
			}
			mode, err := customAssetPacketWitnessMode(
				packet, groupInputs,
			)
			if err != nil {
				return nil, nil, nil, err
			}
			if mode == CustomAssetWitnessBackendSigner {
				if err := b.resolveInputSigningKeys(
					ctx, packet,
				); err != nil {
					return nil, nil, nil, err
				}
			}
			backendSigning = append(
				backendSigning,
				mode == CustomAssetWitnessBackendSigner,
			)
		}

		packets = append(packets, groupPackets...)
	}

	planned, err := mapCustomAnchorOutputs(outputs, packets)
	if err != nil {
		return nil, nil, nil, err
	}

	return packets, planned, backendSigning, nil
}

func (b *CustomAnchorTxBuilder) customAssetScriptKey(ctx context.Context,
	plan CustomAssetScriptPlan, multiIssuance bool) (
	tapsend.ScriptKeyGen, error) {

	switch plan.Mode {
	case CustomAssetScriptWallet:
		if plan.Wallet.KeyLocator != nil {
			return nil, fmt.Errorf("selecting a wallet script key locator " +
				"is not supported by the current tapd API")
		}
		derived, err := b.wallet.client.DeriveScriptKey(ctx)
		if err != nil {
			return nil, fmt.Errorf("derive wallet script key: %w", err)
		}
		key, err := tapAssetScriptKey(*derived)
		if err != nil {
			return nil, err
		}
		if multiIssuance {
			return uniqueScriptKeyGen(key)
		}
		return tapsend.StaticScriptKeyGen(key), nil

	case CustomAssetScriptExternal:
		key, err := tapAssetScriptKey(plan.External.ScriptKey)
		if err != nil {
			return nil, err
		}
		if multiIssuance {
			if len(plan.External.ScriptKey.TapTweak) != 0 {
				return nil, fmt.Errorf("multi-issuance grouped outputs " +
					"cannot preserve a pre-tweaked external script key")
			}
			return uniqueScriptKeyGen(key)
		}
		return tapsend.StaticScriptKeyGen(key), nil

	case CustomAssetScriptOPTrue:
		internalKey, err := btcec.ParsePubKey(
			plan.OPTrue.InternalKey.RawKeyBytes[:],
		)
		if err != nil {
			return nil, err
		}
		return opTrueScriptKeyGen(
			internalKey, plan.OPTrue.InternalKey.KeyLocator,
		), nil

	case CustomAssetScriptBurn:
		// The final proof-of-burn key is derived from the first PrevID of
		// each concrete virtual packet after coin distribution. NUMS is only
		// a placeholder required by the allocation API.
		return tapsend.StaticScriptKeyGen(asset.NUMSScriptKey), nil

	default:
		return nil, fmt.Errorf("unsupported asset script mode %d",
			plan.Mode)
	}
}

// resolveInputSigningKeys carries the authenticated wallet's complete key
// derivation into each backend-signed input. Proofs contain the output key but
// omit wallet metadata. In particular, a received unique script key requires
// its Taproot tweak; a raw key locator alone would produce a BIP-86 signature.
func (b *CustomAnchorTxBuilder) resolveInputSigningKeys(ctx context.Context,
	packet *tappsbt.VPacket) error {

	for idx, input := range packet.Inputs {
		proofKey := schnorr.SerializePubKey(
			input.Asset().ScriptKey.PubKey,
		)
		resolved, err := b.wallet.client.QueryScriptKey(ctx, proofKey)
		if err != nil {
			return fmt.Errorf(
				"resolve input %d signing key: %w", idx, err,
			)
		}
		if resolved == nil || (len(resolved.TapTweak) != 0 &&
			len(resolved.TapTweak) != 32) {

			return fmt.Errorf(
				"input %d signing key is incomplete", idx,
			)
		}
		key, err := tapAssetScriptKey(*resolved)
		if err != nil || key.TweakedScriptKey == nil {
			return fmt.Errorf(
				"input %d signing key metadata is invalid", idx,
			)
		}
		derived := txscript.ComputeTaprootOutputKey(
			key.RawKey.PubKey, key.Tweak,
		)
		keyBytes := schnorr.SerializePubKey(key.PubKey)
		derivedBytes := schnorr.SerializePubKey(derived)
		if !bytes.Equal(keyBytes, proofKey) ||
			!bytes.Equal(derivedBytes, proofKey) {

			return fmt.Errorf(
				"input %d signing key does not match proof",
				idx,
			)
		}
		inputAsset := input.Asset().Copy()
		inputAsset.ScriptKey = key
		packet.SetInputAsset(idx, inputAsset)
	}
	return nil
}

func uniqueScriptKeyGen(base asset.ScriptKey) (tapsend.ScriptKeyGen, error) {
	if base.TweakedScriptKey == nil ||
		base.TweakedScriptKey.RawKey.PubKey == nil {

		return nil, fmt.Errorf("multi-issuance grouped output requires " +
			"the raw asset script key")
	}
	rawKey := base.TweakedScriptKey.RawKey.PubKey
	return func(assetID asset.ID) (asset.ScriptKey, error) {
		return asset.DeriveUniqueScriptKey(
			*rawKey, assetID,
			asset.ScriptKeyDerivationUniquePedersen,
		)
	}, nil
}

func opTrueScriptKeyGen(internalKey *btcec.PublicKey,
	locator KeyLocator) tapsend.ScriptKeyGen {

	return func(assetID asset.ID) (asset.ScriptKey, error) {
		scriptKey, _, err := customAssetOPTrueScriptKey(
			internalKey, locator, assetID,
		)

		return scriptKey, err
	}
}

func customAssetOPTrueScriptKey(internalKey *btcec.PublicKey,
	locator KeyLocator, assetID asset.ID) (asset.ScriptKey,
	*CustomAssetOPTrueSpendInfo, error) {

	opTrue := txscript.NewBaseTapLeaf([]byte{txscript.OP_TRUE})
	uniqueLeaf, err := asset.NewNonSpendableScriptLeaf(
		asset.PedersenVersion, assetID[:],
	)
	if err != nil {
		return asset.ScriptKey{}, nil, err
	}
	tree := txscript.AssembleTaprootScriptTree(opTrue, uniqueLeaf)
	root := tree.RootNode.TapHash()
	outputKey := txscript.ComputeTaprootOutputKey(internalKey, root[:])
	outputKey, err = schnorr.ParsePubKey(schnorr.SerializePubKey(outputKey))
	if err != nil {
		return asset.ScriptKey{}, nil, err
	}

	leafIndex, ok := tree.LeafProofIndex[opTrue.TapHash()]
	if !ok {
		return asset.ScriptKey{}, nil, fmt.Errorf("OP_TRUE leaf proof missing")
	}
	controlBlock := tree.LeafMerkleProofs[leafIndex].ToControlBlock(
		internalKey,
	)
	controlBlockBytes, err := controlBlock.ToBytes()
	if err != nil {
		return asset.ScriptKey{}, nil, fmt.Errorf("encode OP_TRUE control "+
			"block: %w", err)
	}
	internalKeyBytes, err := ParsePubKey(internalKey.SerializeCompressed())
	if err != nil {
		return asset.ScriptKey{}, nil, err
	}

	spendInfo := &CustomAssetOPTrueSpendInfo{
		LeafScript:   cloneBytes(opTrue.Script),
		LeafVersion:  uint8(opTrue.LeafVersion),
		ControlBlock: controlBlockBytes,
		InternalKey:  internalKeyBytes,
	}
	scriptKey := asset.ScriptKey{
		PubKey: outputKey,
		TweakedScriptKey: &asset.TweakedScriptKey{
			RawKey: keychain.KeyDescriptor{
				Family: keychain.KeyFamily(locator.Family),
				Index:  locator.Index,
				PubKey: internalKey,
			},
			Tweak: root[:],
			Type:  asset.ScriptKeyScriptPathExternal,
		},
	}
	parsedScriptKey, err := ParsePubKey(outputKey.SerializeCompressed())
	if err != nil {
		return asset.ScriptKey{}, nil, err
	}
	if err := spendInfo.Validate(parsedScriptKey); err != nil {
		return asset.ScriptKey{}, nil, fmt.Errorf("validate OP_TRUE spend "+
			"info: %w", err)
	}

	return scriptKey, spendInfo, nil
}

func tapAssetScriptKey(scriptKey ScriptKey) (asset.ScriptKey, error) {
	pubKey, err := btcec.ParsePubKey(scriptKey.PubKey[:])
	if err != nil {
		return asset.ScriptKey{}, fmt.Errorf("parse asset script key: %w",
			err)
	}
	result := asset.NewScriptKey(pubKey)

	rawBytes := scriptKey.KeyDesc.RawKeyBytes
	if rawBytes == (PubKey{}) {
		return result, nil
	}
	rawKey, err := btcec.ParsePubKey(rawBytes[:])
	if err != nil {
		return asset.ScriptKey{}, fmt.Errorf("parse raw asset script key: %w",
			err)
	}
	keyType := asset.ScriptKeyBip86
	if len(scriptKey.TapTweak) != 0 {
		keyType = asset.ScriptKeyScriptPathExternal
	}
	result.TweakedScriptKey = &asset.TweakedScriptKey{
		RawKey: keychain.KeyDescriptor{
			Family: keychain.KeyFamily(
				scriptKey.KeyDesc.KeyLocator.Family,
			),
			Index:  scriptKey.KeyDesc.KeyLocator.Index,
			PubKey: rawKey,
		},
		Tweak: cloneBytes(scriptKey.TapTweak),
		Type:  keyType,
	}

	return result, nil
}

func customAnchorSibling(plan CustomAnchorTapscriptPlan) (
	*commitment.TapscriptPreimage, error) {

	if len(plan.SerializedSibling) != 0 {
		preimage, _, err := commitment.MaybeDecodeTapscriptPreimage(
			plan.SerializedSibling,
		)
		return preimage, err
	}
	if len(plan.TapLeaves) == 0 {
		return nil, nil
	}

	leaves := make([]txscript.TapLeaf, len(plan.TapLeaves))
	for idx := range plan.TapLeaves {
		leaves[idx] = txscript.NewBaseTapLeaf(
			cloneBytes(plan.TapLeaves[idx].Script),
		)
	}
	nodes, err := asset.TapTreeNodesFromLeaves(leaves)
	if err != nil {
		return nil, err
	}

	return commitment.NewPreimageFromTapscriptTreeNodes(*nodes)
}

func customAnchorKeyDerivations(key InternalKey, coinType uint32) (
	[]*psbt.Bip32Derivation, []*psbt.TaprootBip32Derivation) {

	if key.KeyLocator == (KeyLocator{}) {
		return nil, nil
	}
	pubKey, err := btcec.ParsePubKey(key.PubKey[:])
	if err != nil {
		return nil, nil
	}
	keyDesc := keychain.KeyDescriptor{
		Family: keychain.KeyFamily(key.KeyLocator.Family),
		Index:  key.KeyLocator.Index,
		PubKey: pubKey,
	}
	bip32, taprootBip32 := tappsbt.Bip32DerivationFromKeyDesc(
		keyDesc, coinType,
	)

	return []*psbt.Bip32Derivation{bip32},
		[]*psbt.TaprootBip32Derivation{taprootBip32}
}

func assetIdentity(a *asset.Asset) (AssetRef, AssetID, AssetType, error) {
	if a == nil {
		return "", AssetID{}, 0, fmt.Errorf("nil asset")
	}

	issuanceID := AssetID(a.ID())
	assetType := AssetType(a.Type)
	var groupKey *PubKey
	if a.GroupKey != nil {
		parsed, err := ParsePubKey(
			a.GroupKey.GroupPubKey.SerializeCompressed(),
		)
		if err != nil {
			return "", AssetID{}, 0, err
		}
		groupKey = &parsed
	}

	return AssetRefFromTypedAsset(issuanceID, groupKey, assetType),
		issuanceID, assetType, nil
}

func applyCustomAssetWitnesses(packet *tappsbt.VPacket,
	inputs []customAnchorInputMaterial) error {

	mode, err := customAssetPacketWitnessMode(packet, inputs)
	if err != nil {
		return err
	}
	witnesses := make(map[asset.PrevID]wire.TxWitness, len(inputs))
	for idx := range packet.Inputs {
		vIn := packet.Inputs[idx]
		matched := customAnchorInputForPrevID(inputs, vIn.PrevID)
		if matched == nil {
			return fmt.Errorf("virtual packet input %d has no request "+
				"mapping", idx)
		}
		witnesses[vIn.PrevID] = cloneWitness(
			matched.request.Witness.Stack,
		)
	}

	if mode == CustomAssetWitnessBackendSigner {
		return nil
	}

	return applyCallerAssetWitnesses(packet, witnesses)
}

func customAssetPacketWitnessMode(packet *tappsbt.VPacket,
	inputs []customAnchorInputMaterial) (CustomAssetWitnessMode, error) {

	mode := CustomAssetWitnessUnspecified
	for idx := range packet.Inputs {
		vIn := packet.Inputs[idx]
		matched := customAnchorInputForPrevID(inputs, vIn.PrevID)
		var inputMode CustomAssetWitnessMode
		if matched != nil {
			inputMode = matched.request.Witness.Mode
		}
		if inputMode == CustomAssetWitnessUnspecified {
			return inputMode, fmt.Errorf("virtual packet input %d has no "+
				"witness mode mapping", idx)
		}
		if mode == CustomAssetWitnessUnspecified {
			mode = inputMode
		} else if mode != inputMode {
			return mode, fmt.Errorf("virtual packet mixes backend and caller " +
				"asset witness modes")
		}
	}

	return mode, nil
}

func customAnchorInputPrevID(
	input customAnchorInputMaterial) asset.PrevID {

	return asset.PrevID{
		OutPoint:  input.proof.OutPoint(),
		ID:        input.proof.Asset.ID(),
		ScriptKey: asset.ToSerialized(input.proof.Asset.ScriptKey.PubKey),
	}
}

func customAnchorInputForPrevID(inputs []customAnchorInputMaterial,
	prevID asset.PrevID) *customAnchorInputMaterial {

	for idx := range inputs {
		if customAnchorInputPrevID(inputs[idx]) == prevID {
			return &inputs[idx]
		}
	}

	return nil
}

func applyCallerAssetWitnesses(packet *tappsbt.VPacket,
	witnesses map[asset.PrevID]wire.TxWitness) error {

	isSplit, err := packet.HasSplitCommitment()
	if err != nil {
		return err
	}
	newAsset := packet.Outputs[0].Asset
	if isSplit {
		splitOut, err := packet.SplitRootOutput()
		if err != nil {
			return err
		}
		newAsset = splitOut.Asset
	}
	if newAsset == nil || len(newAsset.PrevWitnesses) != len(packet.Inputs) {
		return fmt.Errorf("prepared virtual packet witness shape is invalid")
	}

	for idx := range packet.Inputs {
		witness, ok := witnesses[packet.Inputs[idx].PrevID]
		if !ok || len(witness) == 0 {
			return fmt.Errorf("caller witness for virtual input %d is "+
				"missing", idx)
		}
		newAsset.PrevWitnesses[idx].TxWitness = cloneWitness(witness)
	}

	prevAssets := make(commitment.InputSet, len(packet.Inputs))
	for idx := range packet.Inputs {
		prevAssets[packet.Inputs[idx].PrevID] = packet.Inputs[idx].Asset()
	}

	var splitAssets []*commitment.SplitAsset
	if isSplit {
		splitAssets = make([]*commitment.SplitAsset, len(packet.Outputs))
		for idx := range packet.Outputs {
			output := packet.Outputs[idx]
			splitAsset := output.Asset
			if output.Type.IsSplitRoot() {
				splitAsset = output.SplitAsset
			}
			if splitAsset == nil ||
				len(splitAsset.PrevWitnesses) != 1 ||
				splitAsset.PrevWitnesses[0].SplitCommitment == nil {

				return fmt.Errorf("virtual split output %d is incomplete",
					idx)
			}
			splitAsset.PrevWitnesses[0].SplitCommitment.RootAsset =
				*newAsset.Copy()
			splitAssets[idx] = &commitment.SplitAsset{
				Asset:       *splitAsset,
				OutputIndex: output.AnchorOutputIndex,
			}
		}
	}

	if err := vm.ValidateWitnesses(
		newAsset, splitAssets, prevAssets,
	); err != nil {
		return fmt.Errorf("validate caller asset witnesses: %w", err)
	}

	return nil
}

func validateCustomAssetPacketWitnesses(packet *tappsbt.VPacket) error {
	if packet == nil || len(packet.Inputs) == 0 || len(packet.Outputs) == 0 {
		return fmt.Errorf("virtual packet is incomplete")
	}
	isSplit, err := packet.HasSplitCommitment()
	if err != nil {
		return err
	}
	newAsset := packet.Outputs[0].Asset
	if isSplit {
		splitOut, err := packet.SplitRootOutput()
		if err != nil {
			return err
		}
		newAsset = splitOut.Asset
	}
	if newAsset == nil || len(newAsset.PrevWitnesses) != len(packet.Inputs) {
		return fmt.Errorf("virtual packet witness shape is invalid")
	}

	prevAssets := make(commitment.InputSet, len(packet.Inputs))
	for idx := range packet.Inputs {
		if packet.Inputs[idx] == nil || packet.Inputs[idx].Asset() == nil {
			return fmt.Errorf("virtual packet input %d has no asset", idx)
		}
		prevAssets[packet.Inputs[idx].PrevID] = packet.Inputs[idx].Asset()
	}

	var splitAssets []*commitment.SplitAsset
	if isSplit {
		splitAssets = make([]*commitment.SplitAsset, len(packet.Outputs))
		for idx, output := range packet.Outputs {
			if output == nil {
				return fmt.Errorf("virtual packet output %d is nil", idx)
			}
			splitAsset := output.Asset
			if output.Type.IsSplitRoot() {
				splitAsset = output.SplitAsset
			}
			if splitAsset == nil {
				return fmt.Errorf("virtual split output %d is incomplete", idx)
			}
			splitAssets[idx] = &commitment.SplitAsset{
				Asset:       *splitAsset,
				OutputIndex: output.AnchorOutputIndex,
			}
		}
	}

	if err := vm.ValidateWitnesses(newAsset, splitAssets, prevAssets); err != nil {
		return fmt.Errorf("validate virtual packet witnesses: %w", err)
	}

	return nil
}

func (b *CustomAnchorTxBuilder) resolvePassivePackets(ctx context.Context,
	plan CustomAnchorPassiveAssets, active []*tappsbt.VPacket,
	verification *CustomAnchorVerificationResult) (
	[]customAnchorPassiveMaterial, []*tappsbt.VPacket, [][]byte, error) {

	switch plan.Policy {
	case CustomAnchorPassiveReject:
		return nil, nil, nil, nil

	case CustomAnchorPassivePreserve:
		return nil, nil, nil, fmt.Errorf("backend-discovered passive " +
			"preservation is not supported for proof-selected inputs; " +
			"provide every passive packet with caller re-anchor policy")

	case CustomAnchorPassiveCallerReanchor:
		// Continue below after validating the complete caller inventory.

	default:
		return nil, nil, nil, fmt.Errorf("unknown passive asset policy %d",
			plan.Policy)
	}

	materials := make([]customAnchorPassiveMaterial, len(plan.Packets))
	packets := make([]*tappsbt.VPacket, len(plan.Packets))
	encoded := make([][]byte, len(plan.Packets))
	for idx := range plan.Packets {
		planned := plan.Packets[idx]
		packet, err := tappsbt.Decode(planned.VirtualPSBT)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("decode passive packet %d: %w",
				idx, err)
		}
		if packet.Version != tappsbt.V1 {
			return nil, nil, nil, fmt.Errorf("passive packet %d must use "+
				"virtual packet V1", idx)
		}
		if len(packet.Inputs) != 1 || packet.Inputs[0] == nil ||
			len(packet.Outputs) != 1 || packet.Outputs[0] == nil {

			return nil, nil, nil, fmt.Errorf("passive packet %d must have "+
				"exactly one input and one output", idx)
		}

		file, err := proof.DecodeFile(planned.ProofFile)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("decode passive packet %d "+
				"proof file: %w", idx, err)
		}
		lastProof, err := file.LastProof()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read passive packet %d proof "+
				"tip: %w", idx, err)
		}
		input := packet.Inputs[0]
		inputAsset := input.Asset()
		output := packet.Outputs[0]
		outputAsset := output.Asset
		if inputAsset == nil || outputAsset == nil {
			return nil, nil, nil, fmt.Errorf("passive packet %d asset is "+
				"missing", idx)
		}
		if inputAsset.Version != asset.V1 || output.AssetVersion != asset.V1 ||
			outputAsset.Version != asset.V1 || output.Type != tappsbt.TypeSimple ||
			!output.Interactive || output.SplitAsset != nil {

			return nil, nil, nil, fmt.Errorf("passive packet %d must be a "+
				"simple interactive V1 reanchor", idx)
		}
		if inputAsset.LockTime != 0 || inputAsset.RelativeLockTime != 0 ||
			outputAsset.LockTime != 0 || outputAsset.RelativeLockTime != 0 {

			return nil, nil, nil, fmt.Errorf("passive packet %d asset "+
				"timelocks are not supported", idx)
		}

		var requestedProofURL *url.URL
		if planned.ProofDelivery.CourierAddress != "" {
			requestedProofURL, err = url.ParseRequestURI(
				planned.ProofDelivery.CourierAddress,
			)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("parse passive packet %d "+
					"proof courier: %w", idx, err)
			}
		}
		if !customAnchorProofCourierEqual(
			requestedProofURL, output.ProofDeliveryAddress,
		) {

			return nil, nil, nil, fmt.Errorf("passive packet %d proof "+
				"courier does not match requested delivery", idx)
		}

		expectedPrevID := asset.PrevID{
			OutPoint:  lastProof.OutPoint(),
			ID:        lastProof.Asset.ID(),
			ScriptKey: asset.ToSerialized(lastProof.Asset.ScriptKey.PubKey),
		}
		if input.PrevID != expectedPrevID {
			return nil, nil, nil, fmt.Errorf("passive packet %d input does "+
				"not match its proof tip", idx)
		}
		if !inputAsset.DeepEqual(&lastProof.Asset) {
			return nil, nil, nil, fmt.Errorf("passive packet %d input asset "+
				"does not match its proof tip", idx)
		}
		if err := verifyPassiveAssetIdentity(inputAsset, outputAsset); err != nil {
			return nil, nil, nil, fmt.Errorf("passive packet %d: %w", idx,
				err)
		}
		if inputAsset.Amount != planned.Amount || output.Amount != planned.Amount ||
			outputAsset.Amount != planned.Amount {

			return nil, nil, nil, fmt.Errorf("passive packet %d amount does "+
				"not match requested %d", idx, planned.Amount)
		}
		if !inputAsset.ScriptKey.PubKey.IsEqual(output.ScriptKey.PubKey) ||
			!inputAsset.ScriptKey.PubKey.IsEqual(outputAsset.ScriptKey.PubKey) {

			return nil, nil, nil, fmt.Errorf("passive packet %d changed the "+
				"asset script key", idx)
		}
		actualRef, issuanceID, assetType, err := assetIdentity(inputAsset)
		if err != nil {
			return nil, nil, nil, err
		}
		if !planned.AssetRef.Equivalent(actualRef) {
			return nil, nil, nil, fmt.Errorf("passive packet %d asset ref "+
				"does not match", idx)
		}
		if !passiveInputSharesActiveAnchor(input.PrevID.OutPoint, active) {
			return nil, nil, nil, fmt.Errorf("passive packet %d input is not "+
				"co-anchored with an active input", idx)
		}
		if !passiveOutputMatchesActiveAnchor(output, active) {
			return nil, nil, nil, fmt.Errorf("passive packet %d output does "+
				"not reuse an active anchor output", idx)
		}
		if err := validateCustomAssetPacketWitnesses(packet); err != nil {
			return nil, nil, nil, fmt.Errorf("passive packet %d: %w", idx,
				err)
		}

		verified, err := b.wallet.client.VerifyProof(ctx, planned.ProofFile)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("verify passive packet %d proof "+
				"chain: %w", idx, err)
		}
		proofScriptKey, err := ParsePubKey(
			lastProof.Asset.ScriptKey.PubKey.SerializeCompressed(),
		)
		if err != nil {
			return nil, nil, nil, err
		}
		if verified == nil || !verified.Valid || verified.DecodedProof == nil ||
			verified.DecodedProof.IssuanceID != issuanceID ||
			verified.DecodedProof.Amount != planned.Amount ||
			verified.DecodedProof.ScriptKey != proofScriptKey ||
			verified.DecodedProof.Outpoint != outpointFromWire(lastProof.OutPoint()) {

			return nil, nil, nil, fmt.Errorf("passive packet %d backend proof "+
				"summary does not match the locally decoded proof", idx)
		}

		materials[idx] = customAnchorPassiveMaterial{
			requestIndex: uint32(idx),
			request:      planned,
			packet:       packet,
			proof:        lastProof,
			assetRef:     actualRef,
			issuanceID:   issuanceID,
			assetType:    assetType,
		}
		packets[idx] = packet
		encoded[idx] = cloneBytes(planned.VirtualPSBT)
		inputIndex := uint32(idx)
		addCustomAnchorCheck(
			verification, customAnchorCheckProofChain,
			CustomAnchorVerificationScopePassiveAssets,
			CustomAnchorVerificationOriginBackend, &inputIndex, planned.ID,
			"backend verified the passive proof chain",
		)
	}

	for idx := 1; idx < len(packets); idx++ {
		if !samePassiveAnchorOutput(
			packets[0].Outputs[0], packets[idx].Outputs[0],
		) {

			return nil, nil, nil, fmt.Errorf("passive packet %d does not use "+
				"the shared passive anchor output", idx)
		}
	}

	return materials, packets, encoded, nil
}

func verifyPassiveAssetIdentity(input, output *asset.Asset) error {
	if output.Type != input.Type || output.Genesis != input.Genesis ||
		!input.GroupKey.IsEqual(output.GroupKey) {

		return fmt.Errorf("passive output changed immutable asset identity")
	}

	return nil
}

func passiveInputSharesActiveAnchor(outpoint wire.OutPoint,
	active []*tappsbt.VPacket) bool {

	for _, packet := range active {
		for _, input := range packet.Inputs {
			if input != nil && input.PrevID.OutPoint == outpoint {
				return true
			}
		}
	}

	return false
}

func passiveOutputMatchesActiveAnchor(passive *tappsbt.VOutput,
	active []*tappsbt.VPacket) bool {

	for _, packet := range active {
		for _, output := range packet.Outputs {
			if output != nil && samePassiveAnchorOutput(passive, output) {
				return true
			}
		}
	}

	return false
}

func samePassiveAnchorOutput(left, right *tappsbt.VOutput) bool {
	if left.AnchorOutputIndex != right.AnchorOutputIndex ||
		left.AnchorOutputInternalKey == nil ||
		right.AnchorOutputInternalKey == nil ||
		!left.AnchorOutputInternalKey.IsEqual(right.AnchorOutputInternalKey) {

		return false
	}

	return reflect.DeepEqual(
		left.AnchorOutputTapscriptSibling,
		right.AnchorOutputTapscriptSibling,
	)
}

func prepareCustomAnchorPSBT(req *CustomAnchorRequest,
	active, passive []*tappsbt.VPacket) (*psbt.Packet, error) {

	anchor, err := psbt.NewFromRawBytes(
		bytes.NewReader(req.AnchorPSBT), false,
	)
	if err != nil {
		return nil, fmt.Errorf("decode caller anchor PSBT: %w", err)
	}
	if err := validateCustomAnchorTemplate(req, anchor); err != nil {
		return nil, err
	}

	allPackets := append(
		append([]*tappsbt.VPacket(nil), active...), passive...,
	)
	required, err := tapsend.PrepareAnchoringTemplate(allPackets)
	if err != nil {
		return nil, fmt.Errorf("prepare required anchor metadata: %w", err)
	}
	for srcIdx := range required.UnsignedTx.TxIn {
		srcIn := required.UnsignedTx.TxIn[srcIdx]
		dstIdx := findAnchorInput(anchor, srcIn.PreviousOutPoint)
		if dstIdx < 0 {
			return nil, fmt.Errorf("caller anchor PSBT is missing asset "+
				"input %s", srcIn.PreviousOutPoint)
		}
		if err := mergeRequiredInput(
			&anchor.Inputs[dstIdx], &required.Inputs[srcIdx],
		); err != nil {
			return nil, fmt.Errorf("merge anchor input %d metadata: %w",
				dstIdx, err)
		}
	}

	for idx := range required.Outputs {
		if idx >= len(anchor.Outputs) {
			return nil, fmt.Errorf("caller anchor PSBT is missing required "+
				"output %d", idx)
		}
		if err := mergeRequiredOutput(
			&anchor.Outputs[idx], &required.Outputs[idx],
		); err != nil {
			return nil, fmt.Errorf("merge anchor output %d metadata: %w",
				idx, err)
		}
	}
	if err := validateTxIDStableAnchorInputs(anchor); err != nil {
		return nil, err
	}

	return anchor, nil
}

func validateTxIDStableAnchorInputs(anchor *psbt.Packet) error {
	prevOuts, _, err := anchorPrevOuts(anchor)
	if err != nil {
		return fmt.Errorf("validate anchor previous outputs: %w", err)
	}
	for idx, txIn := range anchor.UnsignedTx.TxIn {
		if len(txIn.SignatureScript) != 0 {
			return fmt.Errorf("anchor input %d has a pre-commit scriptSig", idx)
		}
		prevOut := prevOuts.FetchPrevOutput(txIn.PreviousOutPoint)
		if prevOut == nil {
			return fmt.Errorf("anchor input %d previous output is missing", idx)
		}
		if !txscript.IsPayToWitnessPubKeyHash(prevOut.PkScript) &&
			!txscript.IsPayToWitnessScriptHash(prevOut.PkScript) &&
			!txscript.IsPayToTaproot(prevOut.PkScript) {

			return fmt.Errorf("anchor input %d must spend a native SegWit "+
				"v0 or v1 output so finalization cannot change the txid", idx)
		}
	}

	return nil
}

func validateCustomAnchorTemplate(req *CustomAnchorRequest,
	anchor *psbt.Packet) error {

	if anchor == nil || anchor.UnsignedTx == nil {
		return fmt.Errorf("caller anchor PSBT has no unsigned transaction")
	}
	if len(anchor.Inputs) != len(anchor.UnsignedTx.TxIn) ||
		len(anchor.Outputs) != len(anchor.UnsignedTx.TxOut) {

		return fmt.Errorf("caller anchor PSBT input/output maps are invalid")
	}
	for idx := range anchor.Inputs {
		input := &anchor.Inputs[idx]
		if len(input.PartialSigs) != 0 || len(input.FinalScriptSig) != 0 ||
			len(input.FinalScriptWitness) != 0 ||
			len(input.TaprootKeySpendSig) != 0 ||
			len(input.TaprootScriptSpendSig) != 0 {

			return fmt.Errorf("caller anchor PSBT input %d contains "+
				"pre-commit signature or finalization data", idx)
		}
	}

	for idx := range req.Outputs {
		output := req.Outputs[idx]
		if output.AnchorOutputIndex >= uint32(len(anchor.UnsignedTx.TxOut)) {
			return fmt.Errorf("asset output %q anchor index %d is out of "+
				"range", output.ID, output.AnchorOutputIndex)
		}
		txOut := anchor.UnsignedTx.TxOut[output.AnchorOutputIndex]
		if txOut.Value < 0 || uint64(txOut.Value) != output.AnchorValueSat {
			return fmt.Errorf("asset output %q anchor value is %d, want %d",
				output.ID, txOut.Value, output.AnchorValueSat)
		}
	}

	if req.Funding.Mode == CustomAnchorFundingExternalP2AFeeBump {
		index := req.Funding.ExternalP2AFeeBump.P2AOutputIndex
		if err := validateExternalP2AAnchor(anchor, index); err != nil {
			return err
		}
	}

	return nil
}

func validateExternalP2AAnchor(anchor *psbt.Packet, index uint32) error {
	if anchor == nil || anchor.UnsignedTx == nil {
		return fmt.Errorf("external P2A anchor PSBT is required")
	}
	if anchor.UnsignedTx.Version != 3 {
		return fmt.Errorf("external P2A funding requires transaction version 3")
	}
	if index >= uint32(len(anchor.UnsignedTx.TxOut)) {
		return fmt.Errorf("P2A output index %d is out of range", index)
	}
	p2a := anchor.UnsignedTx.TxOut[index]
	if p2a.Value != 0 || !bytes.Equal(p2a.PkScript, canonicalP2AScript) {
		return fmt.Errorf("output %d is not the canonical zero-value P2A "+
			"output", index)
	}
	for outputIndex, output := range anchor.UnsignedTx.TxOut {
		if uint32(outputIndex) == index {
			continue
		}
		if mempool.IsDust(output, mempool.DefaultMinRelayTxFee) {
			return fmt.Errorf("external P2A parent output %d is dust; the "+
				"declared P2A must be the only dust output", outputIndex)
		}
	}

	return nil
}

func mergeRequiredInput(dst, src *psbt.PInput) error {
	if src.WitnessUtxo != nil {
		if dst.WitnessUtxo != nil && !txOutEqual(
			dst.WitnessUtxo, src.WitnessUtxo,
		) {

			return fmt.Errorf("witness UTXO mismatch")
		}
		dst.WitnessUtxo = cloneTxOut(src.WitnessUtxo)
	}
	if dst.SighashType != 0 && src.SighashType != 0 &&
		dst.SighashType != src.SighashType {

		return fmt.Errorf("sighash type mismatch")
	}
	if dst.SighashType == 0 {
		dst.SighashType = src.SighashType
	}
	if err := mergeBytes(
		"taproot internal key", &dst.TaprootInternalKey,
		src.TaprootInternalKey,
	); err != nil {
		return err
	}
	if err := mergeBytes(
		"taproot merkle root", &dst.TaprootMerkleRoot,
		src.TaprootMerkleRoot,
	); err != nil {
		return err
	}
	if err := mergeDerivations(
		"BIP32 derivation", &dst.Bip32Derivation,
		src.Bip32Derivation,
	); err != nil {
		return err
	}
	return mergeDerivations(
		"taproot BIP32 derivation", &dst.TaprootBip32Derivation,
		src.TaprootBip32Derivation,
	)
}

func mergeRequiredOutput(dst, src *psbt.POutput) error {
	if err := mergeBytes(
		"taproot internal key", &dst.TaprootInternalKey,
		src.TaprootInternalKey,
	); err != nil {
		return err
	}
	if err := mergeDerivations(
		"BIP32 derivation", &dst.Bip32Derivation,
		src.Bip32Derivation,
	); err != nil {
		return err
	}
	return mergeDerivations(
		"taproot BIP32 derivation", &dst.TaprootBip32Derivation,
		src.TaprootBip32Derivation,
	)
}

func mergeBytes(label string, dst *[]byte, src []byte) error {
	if len(src) == 0 {
		return nil
	}
	if len(*dst) != 0 && !bytes.Equal(*dst, src) {
		return fmt.Errorf("%s mismatch", label)
	}
	*dst = cloneBytes(src)
	return nil
}

func mergeDerivations[T any](label string, dst *[]T, src []T) error {
	if len(src) == 0 {
		return nil
	}
	if len(*dst) != 0 && !reflect.DeepEqual(*dst, src) {
		return fmt.Errorf("%s mismatch", label)
	}
	*dst = append([]T(nil), src...)
	return nil
}

func mapCustomAnchorOutputs(requestedOutputs []customAnchorOutputMaterial,
	packets []*tappsbt.VPacket) ([]customAnchorPlannedOutput, error) {

	result := make([]customAnchorPlannedOutput, 0)
	mappedAmounts := make(map[uint32]uint64, len(requestedOutputs))
	for packetIdx := range packets {
		packet := packets[packetIdx]
		issuance, err := packet.AssetID()
		if err != nil {
			return nil, err
		}
		if len(packet.Inputs) == 0 || packet.Inputs[0].Asset() == nil {
			return nil, fmt.Errorf("virtual packet %d has no input asset",
				packetIdx)
		}
		_, _, assetType, err := assetIdentity(packet.Inputs[0].Asset())
		if err != nil {
			return nil, err
		}

		for outputIdx := range packet.Outputs {
			vOut := packet.Outputs[outputIdx]
			actualRef, _, _, err := assetIdentity(vOut.Asset)
			if err != nil {
				return nil, err
			}

			var requested *customAnchorOutputMaterial
			for idx := range requestedOutputs {
				candidate := &requestedOutputs[idx]
				if candidate.request.AnchorOutputIndex !=
					vOut.AnchorOutputIndex ||
					!customAnchorAssetRefMatchesIssuance(
						candidate.request.AssetRef, actualRef,
						AssetID(issuance),
					) {

					continue
				}
				var expectedKey asset.ScriptKey
				if candidate.request.Script.Mode == CustomAssetScriptBurn {
					expectedKey = asset.NewScriptKey(asset.DeriveBurnKey(
						packet.Inputs[0].PrevID,
					))
				} else {
					expectedKey, err = candidate.scriptKey(issuance)
					if err != nil {
						return nil, fmt.Errorf("derive logical output %q "+
							"script key: %w", candidate.request.ID, err)
					}
				}
				if expectedKey.PubKey.IsEqual(vOut.ScriptKey.PubKey) {
					requested = candidate
					break
				}
			}
			if requested == nil {
				return nil, fmt.Errorf("virtual packet output %d:%d has no "+
					"logical output mapping", packetIdx, outputIdx)
			}

			scriptKey, err := ParsePubKey(
				vOut.ScriptKey.PubKey.SerializeCompressed(),
			)
			if err != nil {
				return nil, err
			}
			var opTrueSpend *CustomAssetOPTrueSpendInfo
			if requested.request.Script.Mode == CustomAssetScriptOPTrue {
				internalKey, err := btcec.ParsePubKey(
					requested.request.Script.OPTrue.InternalKey.RawKeyBytes[:],
				)
				if err != nil {
					return nil, fmt.Errorf("parse output %q OP_TRUE "+
						"internal key: %w", requested.request.ID, err)
				}
				expectedKey, spendInfo, err := customAssetOPTrueScriptKey(
					internalKey,
					requested.request.Script.OPTrue.InternalKey.KeyLocator,
					issuance,
				)
				if err != nil {
					return nil, fmt.Errorf("derive output %q OP_TRUE "+
						"spend info: %w", requested.request.ID, err)
				}
				if !expectedKey.PubKey.IsEqual(vOut.ScriptKey.PubKey) {
					return nil, fmt.Errorf("output %q OP_TRUE script key "+
						"does not match its spend info", requested.request.ID)
				}
				opTrueSpend = spendInfo
			}

			requestIndex := requested.requestIndex
			mappedAmount, err := checkedCustomAnchorAdd(
				mappedAmounts[requestIndex], vOut.Amount,
			)
			if err != nil {
				return nil, fmt.Errorf("mapped output %q amount: %w",
					requested.request.ID, err)
			}
			mappedAmounts[requestIndex] = mappedAmount

			result = append(result, customAnchorPlannedOutput{
				LogicalOutputID: requested.request.ID,
				RequestIndex:    requestIndex,
				PacketRole:      CustomAnchorPacketRoleActive,
				PacketIndex:     uint32(packetIdx),
				VirtualOutput:   uint32(outputIdx),
				AnchorOutput:    vOut.AnchorOutputIndex,
				AssetRef:        requested.request.AssetRef,
				IssuanceID:      AssetID(issuance),
				AssetType:       assetType,
				Amount:          vOut.Amount,
				AnchorValueSat:  requested.request.AnchorValueSat,
				ScriptKey:       scriptKey,
				ScriptMode:      requested.request.Script.Mode,
				ProofDelivery: CustomAssetProofDelivery{
					RecipientID: requested.request.ProofDelivery.RecipientID,
					CourierAddress: customAnchorProofCourierString(
						vOut.ProofDeliveryAddress,
					),
					OpaqueMetadata: cloneBytes(
						requested.request.ProofDelivery.OpaqueMetadata,
					),
				},
				OPTrueSpend: opTrueSpend,
			})
		}
	}

	for idx := range requestedOutputs {
		requested := requestedOutputs[idx]
		if mappedAmounts[requested.requestIndex] != requested.request.Amount {
			return nil, fmt.Errorf("logical output %q mapped amount %d, want %d",
				requested.request.ID, mappedAmounts[requested.requestIndex],
				requested.request.Amount)
		}
	}

	return result, nil
}

func customAnchorAssetRefMatchesIssuance(requested, semantic AssetRef,
	issuanceID AssetID) bool {

	if requested.Equivalent(semantic) {
		return true
	}
	requestedID, ok := requested.AssetID()
	return ok && requestedID == issuanceID
}

func customAnchorInputSummary(input customAnchorInputMaterial) (
	CustomAnchorAssetInputSummary, error) {

	proofSource, err := customAnchorProofSource(input.request)
	if err != nil {
		return CustomAnchorAssetInputSummary{}, err
	}
	scriptKey, _ := ParsePubKey(
		input.proof.Asset.ScriptKey.PubKey.SerializeCompressed(),
	)
	return CustomAnchorAssetInputSummary{
		LogicalInputID:    input.request.ID,
		LogicalInputIndex: input.requestIndex,
		PacketRole:        CustomAnchorPacketRoleActive,
		AssetRef:          input.request.AssetRef,
		IssuanceID:        input.issuanceID,
		AssetType:         input.assetType,
		AnchorOutpoint:    outpointFromWire(input.proof.OutPoint()),
		ScriptKey:         scriptKey,
		Amount:            input.request.Amount,
		ProofSource:       proofSource,
	}, nil
}

func customAnchorProofSource(input CustomAssetInput) (
	CustomAnchorProofSourceSummary, error) {

	if input.ProofPath != nil {
		contentID, err := input.ProofPath.ContentID()
		if err != nil {
			return CustomAnchorProofSourceSummary{}, fmt.Errorf(
				"proof path content ID: %w", err,
			)
		}
		blob, err := input.ProofPath.MarshalBinary()
		if err != nil {
			return CustomAnchorProofSourceSummary{}, fmt.Errorf(
				"encode proof path: %w", err,
			)
		}

		return CustomAnchorProofSourceSummary{
			Kind:      CustomAnchorProofSourceCompactPath,
			ContentID: contentID,
			Blob:      blob,
		}, nil
	}

	return CustomAnchorProofSourceSummary{
		Kind: CustomAnchorProofSourceConfirmedFile,
		ContentID: customAnchorDigest(
			customAnchorProofFileDigestDomain, input.ProofFile,
		),
		Blob: cloneBytes(input.ProofFile),
	}, nil
}

func mapCustomAnchorInputs(anchor *psbt.Packet,
	inputs []customAnchorInputMaterial,
	packets []*tappsbt.VPacket) ([]CustomAnchorAssetInputSummary, error) {

	result := make([]CustomAnchorAssetInputSummary, len(inputs))
	seen := make([]bool, len(inputs))
	for packetIdx, packet := range packets {
		for inputIdx, virtualInput := range packet.Inputs {
			if virtualInput == nil {
				return nil, fmt.Errorf("virtual packet %d input %d is nil",
					packetIdx, inputIdx)
			}
			matchedIndex := -1
			for idx := range inputs {
				if customAnchorInputPrevID(inputs[idx]) == virtualInput.PrevID {
					matchedIndex = idx
					break
				}
			}
			if matchedIndex < 0 || seen[matchedIndex] {
				return nil, fmt.Errorf("virtual packet %d input %d has no "+
					"unique logical input mapping", packetIdx, inputIdx)
			}
			summary, err := customAnchorInputSummary(inputs[matchedIndex])
			if err != nil {
				return nil, err
			}
			anchorIndex := findAnchorInput(
				anchor, virtualInput.PrevID.OutPoint,
			)
			if anchorIndex < 0 {
				return nil, fmt.Errorf("virtual packet %d input %d has no "+
					"anchor input", packetIdx, inputIdx)
			}
			summary.PacketIndex = uint32(packetIdx)
			summary.VirtualInputIndex = uint32(inputIdx)
			summary.AnchorInputIndex = uint32(anchorIndex)
			result[matchedIndex] = summary
			seen[matchedIndex] = true
		}
	}
	for idx := range seen {
		if !seen[idx] {
			return nil, fmt.Errorf("logical input %q has no virtual mapping",
				inputs[idx].request.ID)
		}
	}

	return result, nil
}

func mapCustomAnchorPassivePackets(anchor *psbt.Packet,
	materials []customAnchorPassiveMaterial) (
	[]CustomAnchorAssetInputSummary, []customAnchorPlannedOutput, error) {

	inputs := make([]CustomAnchorAssetInputSummary, len(materials))
	outputs := make([]customAnchorPlannedOutput, len(materials))
	for idx := range materials {
		material := materials[idx]
		input := material.packet.Inputs[0]
		output := material.packet.Outputs[0]
		anchorInput := findAnchorInput(anchor, input.PrevID.OutPoint)
		if anchorInput < 0 {
			return nil, nil, fmt.Errorf("passive packet %d has no anchor input",
				idx)
		}
		if output.AnchorOutputIndex >= uint32(len(anchor.UnsignedTx.TxOut)) {
			return nil, nil, fmt.Errorf("passive packet %d anchor output is "+
				"out of range", idx)
		}
		anchorValue := anchor.UnsignedTx.TxOut[output.AnchorOutputIndex].Value
		if anchorValue < 0 {
			return nil, nil, fmt.Errorf("passive packet %d anchor output is "+
				"negative", idx)
		}
		scriptKey, err := ParsePubKey(
			material.proof.Asset.ScriptKey.PubKey.SerializeCompressed(),
		)
		if err != nil {
			return nil, nil, err
		}
		proofSource := CustomAnchorProofSourceSummary{
			Kind: CustomAnchorProofSourceConfirmedFile,
			ContentID: customAnchorDigest(
				customAnchorProofFileDigestDomain,
				material.request.ProofFile,
			),
			Blob: cloneBytes(material.request.ProofFile),
		}
		inputs[idx] = CustomAnchorAssetInputSummary{
			LogicalInputID:    material.request.ID,
			LogicalInputIndex: material.requestIndex,
			PacketRole:        CustomAnchorPacketRolePassive,
			PacketIndex:       uint32(idx),
			VirtualInputIndex: 0,
			AnchorInputIndex:  uint32(anchorInput),
			AssetRef:          material.request.AssetRef,
			IssuanceID:        material.issuanceID,
			AssetType:         material.assetType,
			AnchorOutpoint:    outpointFromWire(input.PrevID.OutPoint),
			ScriptKey:         scriptKey,
			Amount:            material.request.Amount,
			ProofSource:       proofSource,
		}
		outputs[idx] = customAnchorPlannedOutput{
			LogicalOutputID: material.request.ID,
			RequestIndex:    material.requestIndex,
			PacketRole:      CustomAnchorPacketRolePassive,
			PacketIndex:     uint32(idx),
			VirtualOutput:   0,
			AnchorOutput:    output.AnchorOutputIndex,
			AssetRef:        material.request.AssetRef,
			IssuanceID:      material.issuanceID,
			AssetType:       material.assetType,
			Amount:          output.Amount,
			AnchorValueSat:  uint64(anchorValue),
			ScriptKey:       scriptKey,
			ScriptMode:      CustomAssetScriptUnspecified,
			ProofDelivery: CustomAssetProofDelivery{
				RecipientID: material.request.ProofDelivery.RecipientID,
				CourierAddress: customAnchorProofCourierString(
					output.ProofDeliveryAddress,
				),
				OpaqueMetadata: cloneBytes(
					material.request.ProofDelivery.OpaqueMetadata,
				),
			},
		}
	}

	return inputs, outputs, nil
}

func customAnchorProofCourierEqual(left, right *url.URL) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}

	return left.String() == right.String()
}

func customAnchorProofCourierString(proofURL *url.URL) string {
	if proofURL == nil {
		return ""
	}

	return proofURL.String()
}

func encodeVirtualPackets(packets []*tappsbt.VPacket) ([][]byte, error) {
	encoded := make([][]byte, len(packets))
	for idx := range packets {
		var err error
		encoded[idx], err = tappsbt.Encode(packets[idx])
		if err != nil {
			return nil, fmt.Errorf("encode virtual packet %d: %w", idx,
				err)
		}
	}
	return encoded, nil
}

func serializePSBT(packet *psbt.Packet) ([]byte, error) {
	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func findAnchorInput(packet *psbt.Packet, outpoint wire.OutPoint) int {
	for idx := range packet.UnsignedTx.TxIn {
		if packet.UnsignedTx.TxIn[idx].PreviousOutPoint == outpoint {
			return idx
		}
	}
	return -1
}

func outpointFromWire(outpoint wire.OutPoint) Outpoint {
	return Outpoint{
		Txid:  [32]byte(outpoint.Hash),
		Index: outpoint.Index,
	}
}

func cloneTxOut(txOut *wire.TxOut) *wire.TxOut {
	if txOut == nil {
		return nil
	}
	return &wire.TxOut{
		Value:    txOut.Value,
		PkScript: cloneBytes(txOut.PkScript),
	}
}

func validateCustomAnchorBitcoinTransaction(packet *psbt.Packet,
	requireFunded bool) error {

	if packet == nil || packet.UnsignedTx == nil {
		return fmt.Errorf("anchor PSBT is required")
	}
	if err := blockchain.CheckTransactionSanity(
		btcutil.NewTx(packet.UnsignedTx),
	); err != nil {
		return fmt.Errorf("Bitcoin transaction sanity: %w", err)
	}
	_, prevOuts, err := anchorPrevOuts(packet)
	if err != nil {
		return err
	}
	var totalInput int64
	for idx := range prevOuts {
		value := prevOuts[idx].Value
		if value < 0 || value > btcutil.MaxSatoshi {
			return fmt.Errorf("anchor input %d value is out of range", idx)
		}
		if totalInput > btcutil.MaxSatoshi-value {
			return fmt.Errorf("anchor input value total is out of range")
		}
		totalInput += value
	}
	if !requireFunded {
		return nil
	}
	var totalOutput int64
	hasZeroValueP2A := false
	for idx, output := range packet.UnsignedTx.TxOut {
		if output.Value < 0 || output.Value > btcutil.MaxSatoshi {
			return fmt.Errorf("anchor output %d value is out of range", idx)
		}
		if totalOutput > btcutil.MaxSatoshi-output.Value {
			return fmt.Errorf("anchor output value total is out of range")
		}
		totalOutput += output.Value
		if output.Value == 0 && bytes.Equal(
			output.PkScript, canonicalP2AScript,
		) {

			hasZeroValueP2A = true
		}
	}
	if totalOutput > totalInput {
		return fmt.Errorf("anchor outputs exceed inputs by %d satoshis",
			totalOutput-totalInput)
	}
	fee := totalInput - totalOutput
	if hasZeroValueP2A && fee != 0 {
		return fmt.Errorf("anchor transaction with a zero-value P2A output "+
			"must pay exactly zero fee, got %d satoshis", fee)
	}

	return nil
}

func customAnchorTransactionFee(packet *psbt.Packet) (uint64, error) {
	if err := validateCustomAnchorBitcoinTransaction(packet, true); err != nil {
		return 0, err
	}

	_, prevOuts, err := anchorPrevOuts(packet)
	if err != nil {
		return 0, err
	}
	var totalInput int64
	for idx := range prevOuts {
		totalInput += prevOuts[idx].Value
	}
	var totalOutput int64
	for _, output := range packet.UnsignedTx.TxOut {
		totalOutput += output.Value
	}

	return uint64(totalInput - totalOutput), nil
}

func txOutEqual(a, b *wire.TxOut) bool {
	return a != nil && b != nil && a.Value == b.Value &&
		bytes.Equal(a.PkScript, b.PkScript)
}

func cloneWitness(witness [][]byte) wire.TxWitness {
	result := make(wire.TxWitness, len(witness))
	for idx := range witness {
		result[idx] = cloneBytes(witness[idx])
	}
	return result
}

func addCustomAnchorCheck(result *CustomAnchorVerificationResult,
	code CustomAnchorVerificationCode, scope CustomAnchorVerificationScope,
	origin CustomAnchorVerificationOrigin, inputIndex *uint32, outputID,
	message string) {

	result.Checks = append(result.Checks, CustomAnchorVerificationCheck{
		Code:       code,
		Scope:      scope,
		Origin:     origin,
		Passed:     true,
		InputIndex: cloneUint32(inputIndex),
		OutputID:   outputID,
		Message:    message,
	})
}

func cloneCustomAnchorVerification(
	result CustomAnchorVerificationResult) CustomAnchorVerificationResult {

	clone := CustomAnchorVerificationResult{
		Checks: append(
			[]CustomAnchorVerificationCheck(nil), result.Checks...,
		),
		Issues: append(
			[]CustomAnchorVerificationIssue(nil), result.Issues...,
		),
	}
	for idx := range clone.Checks {
		clone.Checks[idx].InputIndex = cloneUint32(
			clone.Checks[idx].InputIndex,
		)
		clone.Checks[idx].OutputIndex = cloneUint32(
			clone.Checks[idx].OutputIndex,
		)
	}
	for idx := range clone.Issues {
		clone.Issues[idx].InputIndex = cloneUint32(
			clone.Issues[idx].InputIndex,
		)
		clone.Issues[idx].OutputIndex = cloneUint32(
			clone.Issues[idx].OutputIndex,
		)
	}

	return clone
}
