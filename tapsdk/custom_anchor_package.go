package tapsdk

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
)

const (
	// CustomAnchorTransferPackageVersion is the current on-disk schema
	// version. Unknown versions are rejected instead of being decoded as the
	// current schema.
	CustomAnchorTransferPackageVersion uint16 = 1

	customAnchorPackageMagic = "TAPCAPKG"
	customAnchorBinaryHeader = len(customAnchorPackageMagic) + 2 + 4
)

var (
	customAnchorCommittedDigestDomain = []byte(
		"tap-sdk/custom-anchor/committed-package/v1",
	)
	customAnchorPackageDigestDomain = []byte(
		"tap-sdk/custom-anchor/package/v1",
	)
	customAnchorUnsignedTxDigestDomain = []byte(
		"tap-sdk/custom-anchor/unsigned-tx/v1",
	)
	customAnchorProofFileDigestDomain = []byte(
		"tap-sdk/custom-anchor/proof-file/v1",
	)
)

// CustomAnchorPacketRole identifies which committed virtual-packet collection
// a mapping indexes. Active and passive packet indices are independent.
type CustomAnchorPacketRole uint8

const (
	// CustomAnchorPacketRoleUnknown is not valid in a sealed package.
	CustomAnchorPacketRoleUnknown CustomAnchorPacketRole = iota

	// CustomAnchorPacketRoleActive indexes ActiveVirtualPsbts.
	CustomAnchorPacketRoleActive

	// CustomAnchorPacketRolePassive indexes PassiveVirtualPsbts.
	CustomAnchorPacketRolePassive
)

// CustomAnchorProofSourceKind identifies the persisted source used to select
// an exact asset input.
type CustomAnchorProofSourceKind uint8

const (
	// CustomAnchorProofSourceUnknown is not valid in a sealed package.
	CustomAnchorProofSourceUnknown CustomAnchorProofSourceKind = iota

	// CustomAnchorProofSourceConfirmedFile is a complete confirmed proof file.
	CustomAnchorProofSourceConfirmedFile

	// CustomAnchorProofSourceCompactPath is a checksummed AssetProofPath.
	CustomAnchorProofSourceCompactPath
)

// CustomAnchorProofSourceSummary makes the transfer package self-contained for
// proof-source recovery while binding the potentially large blob by content ID.
type CustomAnchorProofSourceSummary struct {
	Kind      CustomAnchorProofSourceKind `json:"kind"`
	ContentID Hash                        `json:"content_id"`
	Blob      []byte                      `json:"blob"`
}

// CustomAnchorTransferPackage is the persistable post-commit recovery
// boundary for an advanced custom-anchor asset transaction.
//
// Callers should persist this package before handing AnchorPsbt to an external
// signer or broadcaster. Publish/log retries after process restart should be
// driven from the persisted package plus the final signed anchor PSBT bytes.
type CustomAnchorTransferPackage struct {
	// SchemaVersion identifies the persisted package schema.
	SchemaVersion uint16 `json:"schema_version"`

	// PlanID binds the committed package to the immutable inspected plan.
	PlanID Hash `json:"plan_id"`

	// CommittedPackageDigest binds all committed package fields while
	// excluding signature and finalization fields in the anchor PSBT. It is
	// stable as external signatures are applied and is used by signing
	// request IDs.
	CommittedPackageDigest Hash `json:"committed_package_digest"`

	// UnsignedTxDigest binds the complete ordered unsigned anchor
	// transaction, including version, locktime, inputs, sequences, output
	// scripts, and output values.
	UnsignedTxDigest Hash `json:"unsigned_tx_digest"`

	// PackageDigest detects changes to the current persisted snapshot,
	// including signature and finalization fields.
	PackageDigest Hash `json:"package_digest"`

	// CommittedAnchorPsbt is the immutable post-commit BTC-level PSBT used
	// to derive signing requests and verify the current/final PSBT. It keeps
	// signing metadata that standard PSBT finalization removes.
	CommittedAnchorPsbt []byte `json:"committed_anchor_psbt"`

	// AnchorPsbt is the current BTC-level anchor PSBT. It initially equals
	// CommittedAnchorPsbt and is replaced on returned clones as signatures
	// or final witnesses are applied.
	AnchorPsbt []byte `json:"anchor_psbt"`

	// ActiveVirtualPsbts are the committed active virtual asset PSBTs.
	ActiveVirtualPsbts [][]byte `json:"active_virtual_psbts"`

	// PassiveVirtualPsbts are committed passive virtual asset PSBTs that
	// must remain paired with the active PSBTs when publishing or retrying.
	PassiveVirtualPsbts [][]byte `json:"passive_virtual_psbts"`

	// ChangeOutputIndex is the backend-selected change output index, or -1
	// when no change output exists.
	ChangeOutputIndex int32 `json:"change_output_index"`

	// LockedUTXOs are wallet UTXO locks held for backend-funded anchors.
	LockedUTXOs []CustomAnchorLockedUTXO `json:"locked_utxos"`

	// FundingLock preserves caller-supplied lease credentials and requested
	// expiration for recovery. Current tapd still returns no lease IDs.
	FundingLock CustomAnchorFundingLockMetadata `json:"funding_lock"`

	// Funding records the exact high-level funding policy and the actual
	// committed miner fee. It makes fee limits and the P2A output durable
	// across signing, restart, and external broadcast handoffs.
	Funding CustomAnchorFundingSummary `json:"funding"`

	// Inputs summarize the committed asset inputs without exposing
	// taproot-assets implementation types.
	Inputs []CustomAnchorAssetInputSummary `json:"inputs"`

	// Outputs summarize the committed asset outputs without exposing
	// taproot-assets implementation types.
	Outputs []CustomAnchorAssetOutputSummary `json:"outputs"`

	// ProofUpdates carry the proof metadata callers must keep for receiver
	// proof delivery, import, register, export, or retry flows.
	ProofUpdates []CustomAnchorProofUpdate `json:"proof_updates"`

	// SigningPlans select exactly one required external spending path for
	// each anchor input. The committed PSBT alone cannot safely infer that
	// intent when both key and script paths are available.
	SigningPlans []CustomAnchorInputSigningPlan `json:"signing_plans"`

	// BackendManagedInputIndices identifies anchor inputs that the backend
	// added or owns and will finalize through its anchor signer. Together
	// with SigningPlans, these indices must classify every anchor input
	// exactly once.
	BackendManagedInputIndices []uint32 `json:"backend_managed_input_indices"`

	// Publish stores publish/log retry metadata that is independent from
	// the final signed anchor PSBT bytes.
	Publish CustomAnchorPublishMetadata `json:"publish"`
}

// CustomAnchorLockedUTXO identifies a BTC UTXO locked while funding an
// anchor. CommitVirtualPsbts currently returns only the outpoint, so the DTO
// deliberately does not invent lock IDs, values, or expiration metadata.
type CustomAnchorLockedUTXO struct {
	// Outpoint identifies the locked BTC output.
	Outpoint Outpoint `json:"outpoint"`
}

// CustomAnchorFundingLockMetadata preserves the lock inputs known before the
// backend funding call. It does not invent lease IDs absent from tapd's reply.
type CustomAnchorFundingLockMetadata struct {
	CustomLockID          []byte `json:"custom_lock_id"`
	LockExpirationSeconds uint64 `json:"lock_expiration_seconds"`
}

// CustomAnchorFundingSummary binds the committed transaction to the funding
// policy that authorized it.
type CustomAnchorFundingSummary struct {
	// Mode is the high-level funding mode selected by the inspected plan.
	Mode CustomAnchorFundingMode `json:"mode"`

	// P2AOutputIndex is present only for external P2A fee-bump parents.
	P2AOutputIndex *uint32 `json:"p2a_output_index,omitempty"`

	// ActualFeeSat is the exact fee of the committed anchor transaction.
	ActualFeeSat uint64 `json:"actual_fee_sat"`

	// MaxFeeSat is the caller's hard ceiling for wallet-funded anchors. It is
	// zero for caller-funded exact and external zero-fee P2A parents.
	MaxFeeSat uint64 `json:"max_fee_sat"`
}

// CustomAnchorAssetInputSummary describes one committed asset input.
type CustomAnchorAssetInputSummary struct {
	// LogicalInputID is the stable caller-defined input identifier.
	LogicalInputID string `json:"logical_input_id"`

	// LogicalInputIndex is the caller-facing input position before one
	// logical selection is expanded into concrete issuance packets.
	LogicalInputIndex uint32 `json:"logical_input_index"`

	// PacketIndex identifies the committed virtual packet.
	PacketIndex uint32 `json:"packet_index"`

	// PacketRole selects the active or passive packet collection.
	PacketRole CustomAnchorPacketRole `json:"packet_role"`

	// VirtualInputIndex identifies the input within PacketIndex.
	VirtualInputIndex uint32 `json:"virtual_input_index"`

	// AnchorInputIndex identifies the input in the anchor transaction.
	AnchorInputIndex uint32 `json:"anchor_input_index"`

	// AssetRef is the caller-facing asset identity requested for the input.
	AssetRef AssetRef `json:"asset_ref"`

	// IssuanceID is the concrete asset issuance/tranche spent by this
	// input.
	IssuanceID AssetID `json:"issuance_id"`

	// AssetType is the concrete asset type for the input.
	AssetType AssetType `json:"asset_type"`

	// AnchorOutpoint is the BTC output that held the spent asset
	// commitment.
	AnchorOutpoint Outpoint `json:"anchor_outpoint"`

	// ScriptKey is the spent asset script key.
	ScriptKey PubKey `json:"script_key"`

	// Amount is the number of asset units spent.
	Amount uint64 `json:"amount"`

	// ProofSource identifies and persists the confirmed file or compact path
	// that selected this exact input.
	ProofSource CustomAnchorProofSourceSummary `json:"proof_source"`
}

// CustomAnchorAssetOutputSummary describes one committed asset output.
type CustomAnchorAssetOutputSummary struct {
	// LogicalOutputID is the stable caller-defined output identifier.
	LogicalOutputID string `json:"logical_output_id"`

	// LogicalOutputIndex is the caller-facing output position before one
	// logical allocation is expanded into concrete issuance packets.
	LogicalOutputIndex uint32 `json:"logical_output_index"`

	// PacketIndex identifies the committed virtual packet.
	PacketIndex uint32 `json:"packet_index"`

	// PacketRole selects the active or passive packet collection.
	PacketRole CustomAnchorPacketRole `json:"packet_role"`

	// VirtualOutputIndex identifies the output within PacketIndex.
	VirtualOutputIndex uint32 `json:"virtual_output_index"`

	// AnchorOutputIndex is the output index in the anchor transaction.
	AnchorOutputIndex uint32 `json:"anchor_output_index"`

	// AssetRef is the caller-facing asset identity created at this output.
	AssetRef AssetRef `json:"asset_ref"`

	// IssuanceID is the concrete asset issuance/tranche created at this
	// output.
	IssuanceID AssetID `json:"issuance_id"`

	// AssetType is the concrete asset type for the output.
	AssetType AssetType `json:"asset_type"`

	// AnchorOutpoint is the BTC output that holds the new asset commitment.
	AnchorOutpoint Outpoint `json:"anchor_outpoint"`

	// AnchorValueSat is the BTC value assigned to the asset-bearing output.
	AnchorValueSat int64 `json:"anchor_value_sat"`

	// TaprootAssetRoot is the root of the Taproot Asset commitment before
	// combining it with the host-supplied tapscript sibling. Hosts such as
	// SwapDK use this root to compose their policy tree and control blocks.
	TaprootAssetRoot Hash `json:"taproot_asset_root"`

	// TaprootMerkleRoot is the final BIP341 root that combines the Taproot
	// Asset commitment with the optional host-supplied tapscript sibling.
	// It is the tweak committed to by the anchor output key.
	TaprootMerkleRoot Hash `json:"taproot_merkle_root"`

	// ScriptKey is the asset script key for the new output.
	ScriptKey PubKey `json:"script_key"`

	// Amount is the number of asset units created at this output.
	Amount uint64 `json:"amount"`

	// ScriptMode records how the active output script was constructed. Passive
	// reanchors use CustomAssetScriptUnspecified because their key is retained.
	ScriptMode CustomAssetScriptMode `json:"script_mode"`

	// ProofDelivery preserves the complete host-owned delivery metadata.
	ProofDelivery CustomAssetProofDelivery `json:"proof_delivery"`

	// OPTrueSpend contains the exact asset script-path witness data when the
	// output was built with CustomAssetScriptOPTrue. Other output modes leave
	// it nil.
	OPTrueSpend *CustomAssetOPTrueSpendInfo `json:"op_true_spend,omitempty"`
}

// CustomAnchorProofUpdate describes proof data needed after commit.
type CustomAnchorProofUpdate struct {
	// LogicalOutputID identifies the caller-facing output allocation.
	LogicalOutputID string `json:"logical_output_id"`

	// LogicalOutputIndex identifies the caller-facing output allocation.
	LogicalOutputIndex uint32 `json:"logical_output_index"`

	// PacketIndex identifies the committed virtual packet.
	PacketIndex uint32 `json:"packet_index"`

	// PacketRole selects the active or passive packet collection.
	PacketRole CustomAnchorPacketRole `json:"packet_role"`

	// VirtualOutputIndex identifies the output within PacketIndex.
	VirtualOutputIndex uint32 `json:"virtual_output_index"`

	// AnchorOutputIndex identifies the output in the anchor transaction.
	AnchorOutputIndex uint32 `json:"anchor_output_index"`

	// AssetRef is the caller-facing asset identity for the proof update.
	AssetRef AssetRef `json:"asset_ref"`

	// IssuanceID is the concrete asset issuance/tranche in the proof
	// update.
	IssuanceID AssetID `json:"issuance_id"`

	// ScriptKey identifies the receiver asset script key.
	ScriptKey PubKey `json:"script_key"`

	// AnchorOutpoint identifies the anchor output referenced by the proof.
	AnchorOutpoint Outpoint `json:"anchor_outpoint"`

	// ProofBlob is the proof suffix or proof metadata returned by the
	// backend.
	ProofBlob []byte `json:"proof_blob"`

	// ProofDelivery preserves the receiver and host delivery metadata.
	ProofDelivery CustomAssetProofDelivery `json:"proof_delivery"`
}

// CustomAnchorPublishMetadata contains publish/log retry metadata.
type CustomAnchorPublishMetadata struct {
	// SkipAnchorTxBroadcast asks the backend to log without broadcasting
	// the final anchor transaction.
	SkipAnchorTxBroadcast bool `json:"skip_anchor_tx_broadcast"`

	// Label is an optional backend transfer label.
	Label string `json:"label"`

	// ExternalBroadcast records that the caller intends to broadcast, or
	// has already broadcast, the final anchor transaction outside the
	// backend.
	ExternalBroadcast bool `json:"external_broadcast"`
}

// Seal returns a validated deep copy with the current schema and all package
// digests populated. It never mutates caller-owned fields or byte slices.
func (p *CustomAnchorTransferPackage) Seal() (
	*CustomAnchorTransferPackage, error) {

	if p == nil {
		return nil, fmt.Errorf("nil custom anchor transfer package")
	}

	sealed := p.Clone()
	sealed.SchemaVersion = CustomAnchorTransferPackageVersion
	sealed.CommittedPackageDigest = Hash{}
	sealed.UnsignedTxDigest = Hash{}
	sealed.PackageDigest = Hash{}

	anchorPsbt, err := canonicalAnchorPSBT(sealed.AnchorPsbt, false)
	if err != nil {
		return nil, fmt.Errorf("anchor PSBT: %w", err)
	}
	sealed.AnchorPsbt = anchorPsbt
	if len(sealed.CommittedAnchorPsbt) == 0 {
		sealed.CommittedAnchorPsbt = cloneBytes(anchorPsbt)
	} else {
		committed, err := canonicalAnchorPSBT(
			sealed.CommittedAnchorPsbt, false,
		)
		if err != nil {
			return nil, fmt.Errorf("committed anchor PSBT: %w", err)
		}
		if err := compareCommittedAnchorPSBTMetadata(
			committed, anchorPsbt,
		); err != nil {
			return nil, fmt.Errorf("current anchor PSBT: %w", err)
		}
		sealed.CommittedAnchorPsbt = committed
	}
	sort.Slice(sealed.SigningPlans, func(i, j int) bool {
		return sealed.SigningPlans[i].InputIndex <
			sealed.SigningPlans[j].InputIndex
	})
	slices.Sort(sealed.BackendManagedInputIndices)

	if err := sealed.validateStructure(); err != nil {
		return nil, err
	}

	unsignedDigest, err := customAnchorUnsignedTxDigest(
		sealed.CommittedAnchorPsbt,
	)
	if err != nil {
		return nil, err
	}
	sealed.UnsignedTxDigest = unsignedDigest

	committedDigest, err := sealed.committedDigest()
	if err != nil {
		return nil, err
	}
	sealed.CommittedPackageDigest = committedDigest

	if err := sealed.refreshPackageDigest(); err != nil {
		return nil, err
	}
	if err := sealed.Validate(); err != nil {
		return nil, err
	}

	return sealed, nil
}

// Validate checks the package schema, structure, and all three digest
// bindings. A package must be sealed before it can be persisted or used for
// signing.
func (p *CustomAnchorTransferPackage) Validate() error {
	if p == nil {
		return fmt.Errorf("nil custom anchor transfer package")
	}
	if p.SchemaVersion != CustomAnchorTransferPackageVersion {
		return fmt.Errorf(
			"unsupported custom anchor package version %d",
			p.SchemaVersion,
		)
	}
	if err := p.validateStructure(); err != nil {
		return err
	}

	unsignedDigest, err := customAnchorUnsignedTxDigest(
		p.CommittedAnchorPsbt,
	)
	if err != nil {
		return err
	}
	if p.UnsignedTxDigest != unsignedDigest {
		return fmt.Errorf("unsigned transaction digest mismatch")
	}
	committedPacket, err := decodeAnchorPSBT(p.CommittedAnchorPsbt)
	if err != nil {
		return fmt.Errorf("committed anchor PSBT: %w", err)
	}
	currentPacket, err := decodeAnchorPSBT(p.AnchorPsbt)
	if err != nil {
		return fmt.Errorf("anchor PSBT: %w", err)
	}
	if err := compareUnsignedAnchorTransactions(
		committedPacket.UnsignedTx, currentPacket.UnsignedTx,
	); err != nil {
		return fmt.Errorf("current anchor PSBT: %w", err)
	}
	if err := compareCommittedAnchorPSBTMetadata(
		p.CommittedAnchorPsbt, p.AnchorPsbt,
	); err != nil {
		return fmt.Errorf("current anchor PSBT: %w", err)
	}

	committedDigest, err := p.committedDigest()
	if err != nil {
		return err
	}
	if p.CommittedPackageDigest != committedDigest {
		return fmt.Errorf("committed package digest mismatch")
	}

	packageDigest, err := p.currentPackageDigest()
	if err != nil {
		return err
	}
	if p.PackageDigest != packageDigest {
		return fmt.Errorf("package digest mismatch")
	}

	return nil
}

func (p *CustomAnchorTransferPackage) validateStructure() error {
	if p.PlanID == (Hash{}) {
		return fmt.Errorf("plan ID is required")
	}
	if len(p.AnchorPsbt) == 0 {
		return fmt.Errorf("anchor PSBT is required")
	}
	if len(p.CommittedAnchorPsbt) == 0 {
		return fmt.Errorf("committed anchor PSBT is required")
	}
	if len(p.ActiveVirtualPsbts) == 0 {
		return fmt.Errorf(
			"at least one active virtual PSBT is required",
		)
	}
	if p.ChangeOutputIndex < -1 {
		return fmt.Errorf("change output index must be -1 or greater")
	}
	if err := validateCustomAnchorLockID(p.FundingLock.CustomLockID); err != nil {
		return err
	}
	if err := validateCustomAnchorLockExpiration(
		p.FundingLock.LockExpirationSeconds,
	); err != nil {
		return err
	}
	if len(p.FundingLock.CustomLockID) == 0 &&
		(p.FundingLock.LockExpirationSeconds != 0 ||
			len(p.LockedUTXOs) != 0 ||
			len(p.BackendManagedInputIndices) != 0) {

		return fmt.Errorf("backend-funded package requires a custom lock ID")
	}

	anchorPacket, err := decodeAnchorPSBT(p.CommittedAnchorPsbt)
	if err != nil {
		return fmt.Errorf("anchor PSBT: %w", err)
	}
	if err := validateCustomAnchorBitcoinTransaction(
		anchorPacket, true,
	); err != nil {
		return fmt.Errorf("committed anchor transaction: %w", err)
	}
	actualFee, err := customAnchorTransactionFee(anchorPacket)
	if err != nil {
		return fmt.Errorf("committed anchor fee: %w", err)
	}
	if p.Funding.ActualFeeSat != actualFee {
		return fmt.Errorf("funding summary fee is %d, want %d",
			p.Funding.ActualFeeSat, actualFee)
	}
	switch p.Funding.Mode {
	case CustomAnchorFundingWalletFunded:
		if len(p.FundingLock.CustomLockID) == 0 {
			return fmt.Errorf("wallet-funded package requires a custom lock ID")
		}
		if p.Funding.P2AOutputIndex != nil {
			return fmt.Errorf("wallet-funded package cannot declare a P2A " +
				"output")
		}
		if p.Funding.MaxFeeSat == 0 {
			return fmt.Errorf("wallet-funded package requires a maximum fee")
		}
		if p.Funding.ActualFeeSat > p.Funding.MaxFeeSat {
			return fmt.Errorf("committed anchor fee %d exceeds maximum %d",
				p.Funding.ActualFeeSat, p.Funding.MaxFeeSat)
		}

	case CustomAnchorFundingCallerFundedExact:
		if p.Funding.P2AOutputIndex != nil || p.Funding.MaxFeeSat != 0 {
			return fmt.Errorf("caller-funded exact package has invalid " +
				"funding metadata")
		}
		if err := p.rejectBackendFundingMetadata("caller-funded exact"); err != nil {
			return err
		}

	case CustomAnchorFundingExternalP2AFeeBump:
		if p.Funding.P2AOutputIndex == nil {
			return fmt.Errorf("external P2A package requires an output index")
		}
		if p.Funding.MaxFeeSat != 0 || p.Funding.ActualFeeSat != 0 {
			return fmt.Errorf("external P2A parent must have zero fee")
		}
		index := *p.Funding.P2AOutputIndex
		if err := validateExternalP2AAnchor(
			anchorPacket, index,
		); err != nil {
			return fmt.Errorf("external P2A funding metadata: %w", err)
		}
		if err := p.rejectBackendFundingMetadata("external P2A"); err != nil {
			return err
		}

	default:
		return fmt.Errorf("custom anchor package funding mode is required")
	}
	if p.ChangeOutputIndex >= int32(len(anchorPacket.UnsignedTx.TxOut)) {
		return fmt.Errorf("change output index is out of range")
	}

	if err := validatePackagePsbts(
		"active virtual PSBT", p.ActiveVirtualPsbts,
	); err != nil {
		return err
	}
	if err := validatePackagePsbts(
		"passive virtual PSBT", p.PassiveVirtualPsbts,
	); err != nil {
		return err
	}

	inputMappings := make(map[string]struct{}, len(p.Inputs))
	for i, input := range p.Inputs {
		if input.LogicalInputID == "" {
			return fmt.Errorf("input %d logical ID is required", i)
		}
		if err := validatePackageAssetRef(
			"input", i, input.AssetRef,
		); err != nil {
			return err
		}
		if input.Amount == 0 {
			return fmt.Errorf("input %d amount is required", i)
		}
		packetCount, err := customAnchorPacketCount(
			input.PacketRole, p.ActiveVirtualPsbts,
			p.PassiveVirtualPsbts,
		)
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
		if input.PacketIndex >= uint32(packetCount) {
			return fmt.Errorf("input %d packet index is out of range", i)
		}
		if input.AnchorInputIndex >= uint32(
			len(anchorPacket.UnsignedTx.TxIn),
		) {

			return fmt.Errorf(
				"input %d anchor input index is out of range", i,
			)
		}
		if err := validateCustomAnchorProofSource(input); err != nil {
			return fmt.Errorf("input %d proof source: %w", i, err)
		}
		mappingKey := customAnchorMappingKey(
			input.PacketRole, input.PacketIndex, input.VirtualInputIndex,
		)
		if _, ok := inputMappings[mappingKey]; ok {
			return fmt.Errorf("input %d duplicates a virtual input mapping", i)
		}
		inputMappings[mappingKey] = struct{}{}
	}

	outputMappings := make(map[string]struct{}, len(p.Outputs))
	for i, output := range p.Outputs {
		if output.LogicalOutputID == "" {
			return fmt.Errorf("output %d logical ID is required", i)
		}
		if err := validatePackageAssetRef(
			"output", i, output.AssetRef,
		); err != nil {
			return err
		}
		if output.Amount == 0 {
			return fmt.Errorf("output %d amount is required", i)
		}
		if output.AnchorValueSat < 0 {
			return fmt.Errorf(
				"output %d anchor value is negative", i,
			)
		}
		packetCount, err := customAnchorPacketCount(
			output.PacketRole, p.ActiveVirtualPsbts,
			p.PassiveVirtualPsbts,
		)
		if err != nil {
			return fmt.Errorf("output %d: %w", i, err)
		}
		if output.PacketIndex >= uint32(packetCount) {
			return fmt.Errorf("output %d packet index is out of range", i)
		}
		if output.AnchorOutputIndex >= uint32(
			len(anchorPacket.UnsignedTx.TxOut),
		) {

			return fmt.Errorf(
				"output %d anchor output index is out of range", i,
			)
		}
		if output.PacketRole == CustomAnchorPacketRoleActive &&
			output.ScriptMode == CustomAssetScriptUnspecified {

			return fmt.Errorf("output %d script mode is required", i)
		}
		if output.ScriptMode == CustomAssetScriptOPTrue &&
			output.OPTrueSpend == nil {

			return fmt.Errorf("output %d OP_TRUE spend info is required", i)
		}
		if output.ScriptMode != CustomAssetScriptOPTrue &&
			output.OPTrueSpend != nil {

			return fmt.Errorf("output %d OP_TRUE spend info requires OP_TRUE "+
				"script mode", i)
		}
		if output.OPTrueSpend != nil {
			if err := output.OPTrueSpend.Validate(output.ScriptKey); err != nil {
				return fmt.Errorf("output %d OP_TRUE spend info: %w", i,
					err)
			}
		}
		mappingKey := customAnchorMappingKey(
			output.PacketRole, output.PacketIndex,
			output.VirtualOutputIndex,
		)
		if _, ok := outputMappings[mappingKey]; ok {
			return fmt.Errorf("output %d duplicates a virtual output mapping", i)
		}
		outputMappings[mappingKey] = struct{}{}
	}

	updateMappings := make(map[string]struct{}, len(p.ProofUpdates))
	for i, update := range p.ProofUpdates {
		if update.LogicalOutputID == "" {
			return fmt.Errorf("proof update %d logical ID is required", i)
		}
		if err := validatePackageAssetRef(
			"proof update", i, update.AssetRef,
		); err != nil {
			return err
		}
		packetCount, err := customAnchorPacketCount(
			update.PacketRole, p.ActiveVirtualPsbts,
			p.PassiveVirtualPsbts,
		)
		if err != nil {
			return fmt.Errorf("proof update %d: %w", i, err)
		}
		if update.PacketIndex >= uint32(packetCount) {
			return fmt.Errorf(
				"proof update %d packet index is out of range", i,
			)
		}
		if update.AnchorOutputIndex >= uint32(
			len(anchorPacket.UnsignedTx.TxOut),
		) {

			return fmt.Errorf(
				"proof update %d anchor output index is out of "+
					"range", i,
			)
		}
		if len(update.ProofBlob) == 0 {
			return fmt.Errorf("proof update %d proof blob is required", i)
		}
		mappingKey := customAnchorMappingKey(
			update.PacketRole, update.PacketIndex,
			update.VirtualOutputIndex,
		)
		if _, ok := updateMappings[mappingKey]; ok {
			return fmt.Errorf("proof update %d duplicates a virtual output "+
				"mapping", i)
		}
		updateMappings[mappingKey] = struct{}{}
	}
	if err := p.validateSemanticMappings(
		anchorPacket, inputMappings, outputMappings, updateMappings,
	); err != nil {
		return err
	}

	if err := validateCustomAnchorSigningPlans(
		p.SigningPlans, p.BackendManagedInputIndices,
		uint32(len(anchorPacket.UnsignedTx.TxIn)),
	); err != nil {
		return err
	}
	if len(p.FundingLock.CustomLockID) != 0 {
		funding := CustomAnchorFundingPlan{
			Mode: CustomAnchorFundingWalletFunded,
		}
		if err := validateCustomAnchorLockInventory(
			funding, anchorPacket, p.BackendManagedInputIndices,
			p.LockedUTXOs,
		); err != nil {
			return err
		}
	}

	return nil
}

func (p *CustomAnchorTransferPackage) rejectBackendFundingMetadata(
	mode string) error {

	if p.ChangeOutputIndex != -1 ||
		len(p.FundingLock.CustomLockID) != 0 ||
		p.FundingLock.LockExpirationSeconds != 0 ||
		len(p.LockedUTXOs) != 0 ||
		len(p.BackendManagedInputIndices) != 0 {

		return fmt.Errorf("%s package cannot contain backend funding metadata",
			mode)
	}

	return nil
}

func (p *CustomAnchorTransferPackage) validateSemanticMappings(
	anchor *psbt.Packet, inputMappings, outputMappings,
	updateMappings map[string]struct{}) error {

	active, err := decodeVirtualPackets("active", p.ActiveVirtualPsbts)
	if err != nil {
		return err
	}
	passive, err := decodeVirtualPackets("passive", p.PassiveVirtualPsbts)
	if err != nil {
		return err
	}
	collections := []struct {
		role    CustomAnchorPacketRole
		packets []*tappsbt.VPacket
	}{
		{CustomAnchorPacketRoleActive, active},
		{CustomAnchorPacketRolePassive, passive},
	}
	allPackets := append(
		append([]*tappsbt.VPacket(nil), active...), passive...,
	)
	for _, packet := range allPackets {
		if packet.Version != tappsbt.V1 {
			return fmt.Errorf("committed virtual packets must use V1")
		}
		if err := validateCustomAssetPacketWitnesses(packet); err != nil {
			return err
		}
	}
	if err := tapsend.ValidateAnchorInputs(anchor, allPackets, nil); err != nil {
		return fmt.Errorf("package anchor inputs: %w", err)
	}
	if err := tapsend.ValidateAnchorOutputs(
		anchor, allPackets, false,
	); err != nil {
		return fmt.Errorf("package anchor outputs: %w", err)
	}
	// The committed packets already contain the deterministic STXO alt
	// leaves added by tapd. Re-adding them is non-idempotent, so reconstruct
	// the same commitments from the persisted leaves without mutation.
	commitments, err := tapsend.CreateOutputCommitments(
		allPackets, tapsend.WithNoSTXOProofs(),
	)
	if err != nil {
		return fmt.Errorf("package output commitments: %w", err)
	}

	inputByMapping := make(map[string]CustomAnchorAssetInputSummary,
		len(p.Inputs))
	for idx := range p.Inputs {
		input := p.Inputs[idx]
		inputByMapping[customAnchorMappingKey(
			input.PacketRole, input.PacketIndex, input.VirtualInputIndex,
		)] = input
	}
	outputByMapping := make(map[string]CustomAnchorAssetOutputSummary,
		len(p.Outputs))
	for idx := range p.Outputs {
		output := p.Outputs[idx]
		outputByMapping[customAnchorMappingKey(
			output.PacketRole, output.PacketIndex,
			output.VirtualOutputIndex,
		)] = output
	}
	updateByMapping := make(map[string]CustomAnchorProofUpdate,
		len(p.ProofUpdates))
	for idx := range p.ProofUpdates {
		update := p.ProofUpdates[idx]
		updateByMapping[customAnchorMappingKey(
			update.PacketRole, update.PacketIndex,
			update.VirtualOutputIndex,
		)] = update
	}

	txID := anchor.UnsignedTx.TxHash()
	var actualInputs, actualOutputs int
	for _, collection := range collections {
		for packetIndex, packet := range collection.packets {
			for virtualIndex, virtualInput := range packet.Inputs {
				actualInputs++
				key := customAnchorMappingKey(
					collection.role, uint32(packetIndex),
					uint32(virtualIndex),
				)
				summary, ok := inputByMapping[key]
				if !ok {
					return fmt.Errorf("virtual input %d:%d has no package "+
						"mapping", packetIndex, virtualIndex)
				}
				if err := validatePackageInputMapping(
					anchor, virtualInput, summary,
				); err != nil {
					return fmt.Errorf("virtual input %d:%d: %w", packetIndex,
						virtualIndex, err)
				}
			}

			for virtualIndex, virtualOutput := range packet.Outputs {
				actualOutputs++
				key := customAnchorMappingKey(
					collection.role, uint32(packetIndex),
					uint32(virtualIndex),
				)
				summary, ok := outputByMapping[key]
				if !ok {
					return fmt.Errorf("virtual output %d:%d has no package "+
						"mapping", packetIndex, virtualIndex)
				}
				update, ok := updateByMapping[key]
				if !ok {
					return fmt.Errorf("virtual output %d:%d has no proof "+
						"update", packetIndex, virtualIndex)
				}
				if err := validatePackageOutputMapping(
					anchor, txID, packet, virtualOutput, summary, update,
					commitments, allPackets, virtualIndex,
				); err != nil {
					return fmt.Errorf("virtual output %d:%d: %w", packetIndex,
						virtualIndex, err)
				}
			}
		}
	}
	if actualInputs != len(inputMappings) || actualInputs != len(p.Inputs) {
		return fmt.Errorf("package input mappings do not exactly cover virtual " +
			"inputs")
	}
	if actualOutputs != len(outputMappings) || actualOutputs != len(p.Outputs) ||
		actualOutputs != len(updateMappings) ||
		actualOutputs != len(p.ProofUpdates) {

		return fmt.Errorf("package output mappings do not exactly cover virtual " +
			"outputs")
	}

	return nil
}

func validatePackageInputMapping(anchor *psbt.Packet,
	virtualInput *tappsbt.VInput,
	summary CustomAnchorAssetInputSummary) error {

	if virtualInput == nil || virtualInput.Asset() == nil {
		return fmt.Errorf("virtual input asset is missing")
	}
	inputAsset := virtualInput.Asset()
	if inputAsset.ScriptKey.PubKey == nil {
		return fmt.Errorf("virtual input asset script key is missing")
	}
	actualRef, issuanceID, assetType, err := assetIdentity(inputAsset)
	if err != nil {
		return err
	}
	scriptPubKey, err := virtualInput.PrevID.ScriptKey.ToPubKey()
	if err != nil {
		return err
	}
	scriptKey, err := ParsePubKey(scriptPubKey.SerializeCompressed())
	if err != nil {
		return err
	}
	if !scriptPubKey.IsEqual(inputAsset.ScriptKey.PubKey) ||
		virtualInput.PrevID.ID != inputAsset.ID() {

		return fmt.Errorf("virtual input previous ID does not match its asset")
	}
	anchorIndex := findAnchorInput(anchor, virtualInput.PrevID.OutPoint)
	if anchorIndex < 0 || summary.AnchorInputIndex != uint32(anchorIndex) ||
		!customAnchorAssetRefMatchesIssuance(
			summary.AssetRef, actualRef, issuanceID,
		) ||
		summary.IssuanceID != issuanceID || summary.AssetType != assetType ||
		summary.AnchorOutpoint != outpointFromWire(virtualInput.PrevID.OutPoint) ||
		summary.ScriptKey != scriptKey || summary.Amount != inputAsset.Amount {

		return fmt.Errorf("input summary does not match virtual input")
	}

	return nil
}

func validatePackageOutputMapping(anchor *psbt.Packet, txID chainhash.Hash,
	packet *tappsbt.VPacket, virtualOutput *tappsbt.VOutput,
	summary CustomAnchorAssetOutputSummary, update CustomAnchorProofUpdate,
	commitments tappsbt.OutputCommitments, allPackets []*tappsbt.VPacket,
	virtualIndex int) error {

	if virtualOutput == nil || virtualOutput.Asset == nil ||
		virtualOutput.ProofSuffix == nil {

		return fmt.Errorf("virtual output or proof suffix is missing")
	}
	if virtualOutput.ScriptKey.PubKey == nil ||
		virtualOutput.Asset.ScriptKey.PubKey == nil ||
		!virtualOutput.ScriptKey.PubKey.IsEqual(
			virtualOutput.Asset.ScriptKey.PubKey,
		) || virtualOutput.Amount != virtualOutput.Asset.Amount {

		return fmt.Errorf("virtual output metadata does not match its asset")
	}
	if virtualOutput.SplitAsset != nil &&
		(virtualOutput.SplitAsset.ScriptKey.PubKey == nil ||
			!virtualOutput.ScriptKey.PubKey.IsEqual(
				virtualOutput.SplitAsset.ScriptKey.PubKey,
			) || virtualOutput.Amount != virtualOutput.SplitAsset.Amount) {

		return fmt.Errorf("virtual split output metadata does not match its " +
			"asset")
	}
	actualRef, issuanceID, assetType, err := assetIdentity(virtualOutput.Asset)
	if err != nil {
		return err
	}
	isBurn := virtualOutput.Asset.IsBurn()
	if (summary.ScriptMode == CustomAssetScriptBurn) != isBurn {
		return fmt.Errorf("output burn state does not match script mode")
	}
	scriptKey, err := ParsePubKey(
		virtualOutput.ScriptKey.PubKey.SerializeCompressed(),
	)
	if err != nil {
		return err
	}
	anchorIndex := virtualOutput.AnchorOutputIndex
	if anchorIndex >= uint32(len(anchor.UnsignedTx.TxOut)) {
		return fmt.Errorf("anchor output is out of range")
	}
	anchorOutpoint := Outpoint{Txid: [32]byte(txID), Index: anchorIndex}
	anchorValue := anchor.UnsignedTx.TxOut[anchorIndex].Value
	taprootAssetRoot, taprootMerkleRoot, err :=
		deriveCustomAnchorOutputRoots(virtualOutput, commitments)
	if err != nil {
		return fmt.Errorf("derive output commitment roots: %w", err)
	}
	if !customAnchorAssetRefMatchesIssuance(
		summary.AssetRef, actualRef, issuanceID,
	) ||
		summary.IssuanceID != issuanceID || summary.AssetType != assetType ||
		summary.AnchorOutputIndex != anchorIndex ||
		summary.AnchorOutpoint != anchorOutpoint ||
		summary.AnchorValueSat != anchorValue || summary.ScriptKey != scriptKey ||
		summary.Amount != virtualOutput.Amount {

		return fmt.Errorf("output summary does not match virtual output")
	}
	if summary.TaprootAssetRoot != taprootAssetRoot ||
		summary.TaprootMerkleRoot != taprootMerkleRoot {

		return fmt.Errorf("output commitment roots do not match virtual output")
	}
	if err := validateCustomAnchorOutputRootHints(
		anchor, anchorIndex, taprootAssetRoot, taprootMerkleRoot,
	); err != nil {
		return err
	}
	if customAnchorProofCourierString(
		virtualOutput.ProofDeliveryAddress,
	) != summary.ProofDelivery.CourierAddress {

		return fmt.Errorf("output proof courier does not match output summary")
	}
	if update.LogicalOutputID != summary.LogicalOutputID ||
		update.LogicalOutputIndex != summary.LogicalOutputIndex ||
		update.AnchorOutputIndex != summary.AnchorOutputIndex ||
		!update.AssetRef.Equivalent(summary.AssetRef) ||
		update.IssuanceID != summary.IssuanceID ||
		update.ScriptKey != summary.ScriptKey ||
		update.AnchorOutpoint != summary.AnchorOutpoint ||
		update.ProofDelivery.RecipientID != summary.ProofDelivery.RecipientID ||
		update.ProofDelivery.CourierAddress !=
			summary.ProofDelivery.CourierAddress ||
		!bytes.Equal(update.ProofDelivery.OpaqueMetadata,
			summary.ProofDelivery.OpaqueMetadata) {

		return fmt.Errorf("proof update does not match output summary")
	}
	updateProof, err := proof.Decode(update.ProofBlob)
	if err != nil {
		return fmt.Errorf("decode proof update: %w", err)
	}
	if updateProof.IsUnknownVersion() {
		return fmt.Errorf("unsupported proof update transition proof "+
			"version %d", updateProof.Version)
	}
	if virtualOutput.ProofSuffix.IsUnknownVersion() {
		return fmt.Errorf("unsupported virtual output transition proof "+
			"version %d", virtualOutput.ProofSuffix.Version)
	}
	if virtualOutput.ProofSuffix.Version != updateProof.Version {
		return fmt.Errorf("proof suffix transition proof versions do not " +
			"match")
	}
	if err := checkActivationEvidence(updateProof); err != nil {
		return err
	}
	expectedSuffix, err := tapsend.CreateProofSuffix(
		anchor.UnsignedTx, anchor.Outputs, packet, commitments,
		virtualIndex, allPackets, proof.WithVersion(updateProof.Version),
	)
	if err != nil {
		return err
	}
	actualMatches, err := customAnchorTransitionProofsEqual(
		expectedSuffix, virtualOutput.ProofSuffix,
	)
	if err != nil {
		return err
	}
	updateMatches, err := customAnchorTransitionProofsEqual(
		expectedSuffix, updateProof,
	)
	if err != nil {
		return err
	}
	if !actualMatches || !updateMatches {
		return fmt.Errorf("proof suffix does not match committed anchor")
	}

	return nil
}

func deriveCustomAnchorOutputRoots(output *tappsbt.VOutput,
	commitments tappsbt.OutputCommitments) (Hash, Hash, error) {

	if output == nil {
		return Hash{}, Hash{}, fmt.Errorf("virtual output is missing")
	}
	if output.AnchorOutputInternalKey == nil {
		return Hash{}, Hash{}, fmt.Errorf("anchor output internal key is missing")
	}
	anchorCommitment, ok := commitments[output.AnchorOutputIndex]
	if !ok || anchorCommitment == nil {
		return Hash{}, Hash{}, fmt.Errorf(
			"anchor output %d commitment is missing",
			output.AnchorOutputIndex,
		)
	}

	_, merkleRoot, assetRoot, err := tapsend.AnchorOutputScript(
		output.AnchorOutputInternalKey,
		output.AnchorOutputTapscriptSibling, anchorCommitment,
	)
	if err != nil {
		return Hash{}, Hash{}, err
	}

	return Hash(assetRoot), Hash(merkleRoot), nil
}

func validateCustomAnchorOutputRootHints(anchor *psbt.Packet,
	anchorIndex uint32, assetRoot, merkleRoot Hash) error {

	if anchorIndex >= uint32(len(anchor.Outputs)) {
		return fmt.Errorf("anchor output metadata is out of range")
	}
	unknowns := anchor.Outputs[anchorIndex].Unknowns
	assetRootHint := tappsbt.ExtractCustomField(
		unknowns, tappsbt.PsbtKeyTypeOutputAssetRoot,
	)
	if len(assetRootHint) != len(assetRoot) {
		return fmt.Errorf("anchor output %d Taproot Asset root hint must be %d "+
			"bytes, was %d", anchorIndex, len(assetRoot), len(assetRootHint))
	}
	if !bytes.Equal(assetRootHint, assetRoot[:]) {
		return fmt.Errorf("anchor output %d Taproot Asset root hint does not "+
			"match the committed assets", anchorIndex)
	}

	merkleRootHint := tappsbt.ExtractCustomField(
		unknowns, tappsbt.PsbtKeyTypeOutputTaprootMerkleRoot,
	)
	if len(merkleRootHint) != len(merkleRoot) {
		return fmt.Errorf("anchor output %d Taproot merkle root hint must be %d "+
			"bytes, was %d", anchorIndex, len(merkleRoot),
			len(merkleRootHint))
	}
	if !bytes.Equal(merkleRootHint, merkleRoot[:]) {
		return fmt.Errorf("anchor output %d Taproot merkle root hint does not "+
			"match the committed assets", anchorIndex)
	}

	return nil
}

// Clone returns a deep copy of the package so callers can snapshot a committed
// state before external signing or broadcast mutates host-owned buffers.
func (p *CustomAnchorTransferPackage) Clone() *CustomAnchorTransferPackage {
	if p == nil {
		return nil
	}

	clone := *p
	clone.CommittedAnchorPsbt = cloneBytes(p.CommittedAnchorPsbt)
	clone.AnchorPsbt = cloneBytes(p.AnchorPsbt)
	clone.ActiveVirtualPsbts = cloneByteSlices(p.ActiveVirtualPsbts)
	clone.PassiveVirtualPsbts = cloneByteSlices(p.PassiveVirtualPsbts)
	clone.LockedUTXOs = append(
		[]CustomAnchorLockedUTXO(nil), p.LockedUTXOs...,
	)
	clone.FundingLock.CustomLockID = cloneBytes(
		p.FundingLock.CustomLockID,
	)
	clone.Funding.P2AOutputIndex = cloneUint32(p.Funding.P2AOutputIndex)
	clone.Inputs = cloneCustomAnchorInputSummaries(p.Inputs)
	clone.Outputs = cloneCustomAnchorOutputSummaries(p.Outputs)
	clone.ProofUpdates = cloneProofUpdates(p.ProofUpdates)
	clone.SigningPlans = cloneCustomAnchorSigningPlans(p.SigningPlans)
	clone.BackendManagedInputIndices = append(
		[]uint32(nil), p.BackendManagedInputIndices...,
	)

	return &clone
}

// MarshalJSON returns the deterministic JSON representation of a validated
// package.
func (p *CustomAnchorTransferPackage) MarshalJSON() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}

	type packageJSON CustomAnchorTransferPackage
	// The package DTO has explicit tags on its wire fields. The alias avoids
	// recursively invoking this method.
	//nolint:musttag
	return json.Marshal((*packageJSON)(p))
}

// UnmarshalJSON decodes a package and rejects unknown fields, unsupported
// schema versions, and digest mismatches.
func (p *CustomAnchorTransferPackage) UnmarshalJSON(data []byte) error {
	if p == nil {
		return fmt.Errorf("nil custom anchor transfer package")
	}

	type packageJSON CustomAnchorTransferPackage
	var decoded packageJSON
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	// The package DTO has explicit tags on its wire fields. The alias avoids
	// recursively invoking UnmarshalJSON.
	//nolint:musttag
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("decode custom anchor package: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}

	temporary := CustomAnchorTransferPackage(decoded)
	if err := temporary.Validate(); err != nil {
		return err
	}
	*p = temporary

	return nil
}

// MarshalBinary returns a versioned deterministic binary envelope containing
// the canonical JSON package.
func (p *CustomAnchorTransferPackage) MarshalBinary() ([]byte, error) {
	jsonBytes, err := p.MarshalJSON()
	if err != nil {
		return nil, err
	}
	if uint64(len(jsonBytes)) > math.MaxUint32 {
		return nil, fmt.Errorf("custom anchor package is too large")
	}

	result := make([]byte, customAnchorBinaryHeader+len(jsonBytes))
	copy(result, customAnchorPackageMagic)
	binary.BigEndian.PutUint16(
		result[len(customAnchorPackageMagic):], p.SchemaVersion,
	)
	binary.BigEndian.PutUint32(
		result[len(customAnchorPackageMagic)+2:], uint32(len(jsonBytes)),
	)
	copy(result[customAnchorBinaryHeader:], jsonBytes)

	return result, nil
}

// UnmarshalBinary decodes a versioned binary envelope and validates both its
// header and embedded package.
func (p *CustomAnchorTransferPackage) UnmarshalBinary(data []byte) error {
	if p == nil {
		return fmt.Errorf("nil custom anchor transfer package")
	}
	if len(data) < customAnchorBinaryHeader {
		return fmt.Errorf("custom anchor package binary envelope is short")
	}
	if string(data[:len(customAnchorPackageMagic)]) !=
		customAnchorPackageMagic {

		return fmt.Errorf("invalid custom anchor package binary magic")
	}

	versionOffset := len(customAnchorPackageMagic)
	version := binary.BigEndian.Uint16(data[versionOffset:])
	if version != CustomAnchorTransferPackageVersion {
		return fmt.Errorf(
			"unsupported custom anchor package version %d", version,
		)
	}

	lengthOffset := versionOffset + 2
	payloadLength := binary.BigEndian.Uint32(data[lengthOffset:])
	if uint64(payloadLength) != uint64(len(data)-customAnchorBinaryHeader) {
		return fmt.Errorf("custom anchor package binary length mismatch")
	}

	var decoded CustomAnchorTransferPackage
	if err := json.Unmarshal(
		data[customAnchorBinaryHeader:], &decoded,
	); err != nil {
		return err
	}
	if decoded.SchemaVersion != version {
		return fmt.Errorf("custom anchor package header version mismatch")
	}
	*p = decoded

	return nil
}

func compareCommittedAnchorPSBTMetadata(committed, current []byte) error {
	currentPacket, err := decodeAnchorPSBT(current)
	if err != nil {
		return fmt.Errorf("decode current anchor PSBT: %w", err)
	}
	hasFinalization := false
	for idx := range currentPacket.Inputs {
		if len(currentPacket.Inputs[idx].FinalScriptSig) != 0 ||
			len(currentPacket.Inputs[idx].FinalScriptWitness) != 0 {

			hasFinalization = true
			break
		}
	}
	if hasFinalization {
		// PSBT serialization intentionally omits non-final input metadata once
		// a final script is present. Rebuild the expected finalized form from
		// the immutable committed packet, then compare the complete canonical
		// encodings. Consensus verification and signing-plan enforcement also
		// use the committed prevouts, never the lossy current input maps.
		expected, _, err := sanitizeFinalAnchorPSBT(committed, current)
		if err != nil {
			return err
		}
		expectedCanonical, err := canonicalAnchorPSBT(expected, false)
		if err != nil {
			return fmt.Errorf("canonical finalized anchor PSBT: %w", err)
		}
		currentCanonical, err := canonicalAnchorPSBT(current, false)
		if err != nil {
			return fmt.Errorf("canonical current anchor PSBT: %w", err)
		}
		if !bytes.Equal(expectedCanonical, currentCanonical) {
			return fmt.Errorf("metadata changed outside finalization fields")
		}

		return nil
	}

	committedCanonical, err := canonicalAnchorPSBT(committed, true)
	if err != nil {
		return fmt.Errorf("canonical committed anchor PSBT: %w", err)
	}
	currentCanonical, err := canonicalAnchorPSBT(current, true)
	if err != nil {
		return fmt.Errorf("canonical current anchor PSBT: %w", err)
	}
	if !bytes.Equal(committedCanonical, currentCanonical) {
		difference := min(len(committedCanonical), len(currentCanonical))
		for idx := 0; idx < difference; idx++ {
			if committedCanonical[idx] != currentCanonical[idx] {
				difference = idx
				break
			}
		}
		return fmt.Errorf("metadata changed outside signature and finalization "+
			"fields (committed length %d, current length %d, first "+
			"difference %d)", len(committedCanonical),
			len(currentCanonical), difference)
	}

	return nil
}

func (p *CustomAnchorTransferPackage) committedDigest() (Hash, error) {
	clone := p.Clone()
	clone.CommittedPackageDigest = Hash{}
	clone.UnsignedTxDigest = Hash{}
	clone.PackageDigest = Hash{}

	anchorPsbt, err := canonicalAnchorPSBT(
		clone.CommittedAnchorPsbt, true,
	)
	if err != nil {
		return Hash{}, fmt.Errorf("anchor PSBT: %w", err)
	}
	clone.CommittedAnchorPsbt = cloneBytes(anchorPsbt)
	clone.AnchorPsbt = anchorPsbt

	type packageJSON CustomAnchorTransferPackage
	//nolint:musttag // The package DTO owns the persisted JSON tags.
	payload, err := json.Marshal((*packageJSON)(clone))
	if err != nil {
		return Hash{}, fmt.Errorf("encode committed package digest: %w", err)
	}

	return customAnchorDigest(customAnchorCommittedDigestDomain, payload), nil
}

func (p *CustomAnchorTransferPackage) currentPackageDigest() (Hash, error) {
	clone := p.Clone()
	clone.PackageDigest = Hash{}

	anchorPsbt, err := canonicalAnchorPSBT(clone.AnchorPsbt, false)
	if err != nil {
		return Hash{}, fmt.Errorf("anchor PSBT: %w", err)
	}
	clone.AnchorPsbt = anchorPsbt

	type packageJSON CustomAnchorTransferPackage
	//nolint:musttag // The package DTO owns the persisted JSON tags.
	payload, err := json.Marshal((*packageJSON)(clone))
	if err != nil {
		return Hash{}, fmt.Errorf("encode package digest: %w", err)
	}

	return customAnchorDigest(customAnchorPackageDigestDomain, payload), nil
}

func (p *CustomAnchorTransferPackage) refreshPackageDigest() error {
	digest, err := p.currentPackageDigest()
	if err != nil {
		return err
	}

	p.PackageDigest = digest
	return nil
}

func customAnchorDigest(domain, payload []byte) Hash {
	hasher := sha256.New()
	_, _ = hasher.Write(domain)
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(payload)

	var digest Hash
	copy(digest[:], hasher.Sum(nil))
	return digest
}

func validatePackagePsbts(label string, psbts [][]byte) error {
	for i, psbt := range psbts {
		if len(psbt) == 0 {
			return fmt.Errorf("%s %d is empty", label, i)
		}
	}

	return nil
}

func validatePackageAssetRef(scope string, index int, ref AssetRef) error {
	if ref.IsZero() {
		return fmt.Errorf("%s %d asset ref is required", scope, index)
	}
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("%s %d asset ref: %w", scope, index, err)
	}

	return nil
}

func customAnchorPacketCount(role CustomAnchorPacketRole, active,
	passive [][]byte) (int, error) {

	switch role {
	case CustomAnchorPacketRoleActive:
		return len(active), nil

	case CustomAnchorPacketRolePassive:
		return len(passive), nil

	default:
		return 0, fmt.Errorf("unknown packet role %d", role)
	}
}

func customAnchorMappingKey(role CustomAnchorPacketRole, packetIndex,
	virtualIndex uint32) string {

	var key [9]byte
	key[0] = byte(role)
	binary.BigEndian.PutUint32(key[1:5], packetIndex)
	binary.BigEndian.PutUint32(key[5:9], virtualIndex)

	return string(key[:])
}

func validateCustomAnchorProofSource(
	input CustomAnchorAssetInputSummary) error {

	if len(input.ProofSource.Blob) == 0 {
		return fmt.Errorf("proof source blob is required")
	}

	var tip *proof.Proof
	switch input.ProofSource.Kind {
	case CustomAnchorProofSourceConfirmedFile:
		expectedID := customAnchorDigest(
			customAnchorProofFileDigestDomain, input.ProofSource.Blob,
		)
		if input.ProofSource.ContentID != expectedID {
			return fmt.Errorf("confirmed proof content ID mismatch")
		}
		file, err := proof.DecodeFile(input.ProofSource.Blob)
		if err != nil {
			return fmt.Errorf("decode confirmed proof file: %w", err)
		}
		tip, err = file.LastProof()
		if err != nil {
			return fmt.Errorf("read confirmed proof tip: %w", err)
		}

	case CustomAnchorProofSourceCompactPath:
		var path AssetProofPath
		if err := path.UnmarshalBinary(input.ProofSource.Blob); err != nil {
			return fmt.Errorf("decode compact proof path: %w", err)
		}
		expectedID, err := path.ContentID()
		if err != nil {
			return err
		}
		if input.ProofSource.ContentID != expectedID {
			return fmt.Errorf("compact proof path content ID mismatch")
		}
		if len(path.Steps) == 0 {
			tip, err = decodeAssetProofPathBase(path.ConfirmedBaseProof)
		} else {
			last := len(path.Steps) - 1
			tip, err = decodeAssetProofPathStep(
				&path.Steps[last], path.stepWitnessCount(last),
			)
		}
		if err != nil {
			return fmt.Errorf("decode compact proof path tip: %w", err)
		}

	default:
		return fmt.Errorf("unknown proof source kind %d",
			input.ProofSource.Kind)
	}

	actualRef, issuanceID, assetType, err := assetIdentity(&tip.Asset)
	if err != nil {
		return err
	}
	scriptKey, err := ParsePubKey(
		tip.Asset.ScriptKey.PubKey.SerializeCompressed(),
	)
	if err != nil {
		return err
	}
	if !customAnchorAssetRefMatchesIssuance(
		input.AssetRef, actualRef, issuanceID,
	) ||
		input.IssuanceID != issuanceID || input.AssetType != assetType ||
		input.AnchorOutpoint != outpointFromWire(tip.OutPoint()) ||
		input.ScriptKey != scriptKey || input.Amount != tip.Asset.Amount {

		return fmt.Errorf("proof source tip does not match input summary")
	}

	return nil
}

func (p *CustomAnchorTransferPackage) rejectUnconfirmedProofPaths() error {
	for idx := range p.Inputs {
		source := p.Inputs[idx].ProofSource
		if source.Kind != CustomAnchorProofSourceCompactPath {
			continue
		}
		var path AssetProofPath
		if err := path.UnmarshalBinary(source.Blob); err != nil {
			return fmt.Errorf("input %d compact proof path: %w", idx, err)
		}
		if len(path.Steps) != 0 {
			return fmt.Errorf("input %d: %w", idx,
				ErrUnconfirmedCustomAnchorPublish)
		}
	}

	return nil
}

func cloneBytes(src []byte) []byte {
	if src == nil {
		return nil
	}

	return append([]byte(nil), src...)
}

func cloneByteSlices(src [][]byte) [][]byte {
	if src == nil {
		return nil
	}

	clone := make([][]byte, len(src))
	for i := range src {
		clone[i] = cloneBytes(src[i])
	}

	return clone
}

func cloneProofUpdates(
	src []CustomAnchorProofUpdate) []CustomAnchorProofUpdate {

	if src == nil {
		return nil
	}

	clone := append([]CustomAnchorProofUpdate(nil), src...)
	for i := range clone {
		clone[i].ProofBlob = cloneBytes(src[i].ProofBlob)
		clone[i].ProofDelivery.OpaqueMetadata = cloneBytes(
			src[i].ProofDelivery.OpaqueMetadata,
		)
	}

	return clone
}

func cloneCustomAnchorInputSummaries(
	src []CustomAnchorAssetInputSummary) []CustomAnchorAssetInputSummary {

	if src == nil {
		return nil
	}

	clone := append([]CustomAnchorAssetInputSummary(nil), src...)
	for idx := range clone {
		clone[idx].ProofSource.Blob = cloneBytes(
			src[idx].ProofSource.Blob,
		)
	}

	return clone
}

func cloneCustomAnchorOutputSummaries(
	src []CustomAnchorAssetOutputSummary) []CustomAnchorAssetOutputSummary {

	if src == nil {
		return nil
	}

	clone := append([]CustomAnchorAssetOutputSummary(nil), src...)
	for idx := range clone {
		clone[idx].OPTrueSpend = src[idx].OPTrueSpend.Clone()
		clone[idx].ProofDelivery.OpaqueMetadata = cloneBytes(
			src[idx].ProofDelivery.OpaqueMetadata,
		)
	}

	return clone
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode custom anchor package: %w", err)
	}

	return fmt.Errorf("decode custom anchor package: trailing value")
}
