package tapsdk

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var customAnchorPlanDigestDomain = []byte(
	"tap-sdk/custom-anchor/plan/v1",
)

// CustomAnchorCommitOptions contains metadata that must be bound into the
// persisted package before external signing begins.
type CustomAnchorCommitOptions struct {
	// Publish is the publish/log policy persisted for crash recovery.
	Publish CustomAnchorPublishMetadata
}

// CustomAnchorCommitResponseError reports a local verification or persistence
// failure after the backend returned a commit response. LockedUTXOs must be
// released explicitly before abandoning the attempt.
type CustomAnchorCommitResponseError struct {
	Err         error
	LockedUTXOs []CustomAnchorLockedUTXO
}

// CustomAnchorCommitAttemptError reports a commit call that returned no
// usable response. If OutcomeUnknown is true, the backend may have committed
// the transfer and retained wallet leases before the transport failed. The
// caller must reconcile those leases before retrying.
type CustomAnchorCommitAttemptError struct {
	Err            error
	FundingLock    CustomAnchorFundingLockMetadata
	OutcomeUnknown bool
}

// CustomAnchorPublishAttemptError reports a publish/log call that returned no
// usable response. If OutcomeUnknown is true, the backend may already have
// logged or broadcast the transfer and the caller must reconcile state before
// retrying.
type CustomAnchorPublishAttemptError struct {
	Err            error
	OutcomeUnknown bool
}

// Error returns the publish-attempt failure and highlights ambiguous results.
func (e *CustomAnchorPublishAttemptError) Error() string {
	if e == nil || e.Err == nil {
		return "custom anchor publish attempt failed"
	}
	if e.OutcomeUnknown {
		return "custom anchor publish outcome is unknown: " + e.Err.Error()
	}

	return "custom anchor publish attempt failed: " + e.Err.Error()
}

// Unwrap returns the underlying transport or backend error.
func (e *CustomAnchorPublishAttemptError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

// Error returns the commit-attempt failure and highlights ambiguous results.
func (e *CustomAnchorCommitAttemptError) Error() string {
	if e == nil || e.Err == nil {
		return "custom anchor commit attempt failed"
	}
	if e.OutcomeUnknown {
		return "custom anchor commit outcome is unknown: " + e.Err.Error()
	}

	return "custom anchor commit attempt failed: " + e.Err.Error()
}

// Unwrap returns the underlying transport or backend error.
func (e *CustomAnchorCommitAttemptError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

// Error returns the post-response commit failure.
func (e *CustomAnchorCommitResponseError) Error() string {
	if e == nil || e.Err == nil {
		return "custom anchor commit response failed verification"
	}

	return "custom anchor commit response failed verification: " +
		e.Err.Error()
}

// Unwrap returns the underlying local failure.
func (e *CustomAnchorCommitResponseError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

// ID returns the deterministic identifier for the exact inspected plan.
func (p *CustomAnchorPlan) ID() (Hash, error) {
	if p == nil || p.request == nil {
		return Hash{}, fmt.Errorf("nil custom anchor plan")
	}
	if err := p.request.Validate(); err != nil {
		return Hash{}, fmt.Errorf("invalid custom anchor plan request: %w",
			err)
	}

	var payload bytes.Buffer
	// The request is an in-memory plan input, not an external JSON schema.
	// JSON provides deterministic field ordering for the plan commitment.
	//nolint:musttag
	requestBytes, err := json.Marshal(p.request)
	if err != nil {
		return Hash{}, fmt.Errorf("encode custom anchor plan request: %w",
			err)
	}
	writePlanDigestBytes(&payload, requestBytes)
	writePlanDigestBytes(&payload, p.anchorPSBT)
	for idx := range p.activeVirtualPSBTs {
		writePlanDigestBytes(&payload, p.activeVirtualPSBTs[idx])
	}
	for idx := range p.passiveVirtualPSBTs {
		writePlanDigestBytes(&payload, p.passiveVirtualPSBTs[idx])
	}
	return customAnchorDigest(customAnchorPlanDigestDomain, payload.Bytes()),
		nil
}

// Commit authorizes backend-managed virtual inputs, asks tapd to bind all
// virtual packets into the anchor transaction, verifies the complete response
// locally, and returns a sealed recovery package. The optional argument may be
// supplied at most once.
//
//nolint:nonamedreturns // The named error annotates all post-response failures.
func (p *CustomAnchorPlan) Commit(ctx context.Context,
	options ...CustomAnchorCommitOptions) (result *CustomAnchorTransferPackage,
	err error) {

	if p == nil || p.client == nil || p.request == nil {
		return nil, fmt.Errorf("nil custom anchor plan")
	}
	if len(options) > 1 {
		return nil, fmt.Errorf("at most one custom anchor commit option is " +
			"allowed")
	}
	var opts CustomAnchorCommitOptions
	if len(options) == 1 {
		opts = options[0]
	}
	if p.request.Funding.Mode ==
		CustomAnchorFundingExternalP2AFeeBump {

		// A zero-fee P2A parent cannot be broadcast on its own. Bind the
		// external package-relay policy into the durable package even when
		// the caller omits the optional metadata argument.
		opts.Publish.SkipAnchorTxBroadcast = true
		opts.Publish.ExternalBroadcast = true
	}
	if !p.verification.Valid() {
		return nil, fmt.Errorf("custom anchor plan verification is not valid")
	}
	advancedClient, err := requireCustomAnchorWalletKitClient(
		p.client, CustomAnchorCapabilityCommit,
	)
	if err != nil {
		return nil, err
	}

	if err := requireCustomAnchorCommitCapabilities(
		ctx, p.client, p.request.Funding, opts.Publish,
	); err != nil {
		return nil, err
	}

	active := cloneByteSlices(p.activeVirtualPSBTs)
	if len(active) != len(p.backendSigning) {
		return nil, fmt.Errorf("custom anchor plan signing classification " +
			"is incomplete")
	}
	for idx := range active {
		if !p.backendSigning[idx] {
			continue
		}
		signed, err := p.client.SignVirtualPsbt(ctx, active[idx])
		if err != nil {
			return nil, fmt.Errorf("sign virtual packet %d: %w", idx, err)
		}
		if err := verifyBackendSignedVirtualPacket(
			active[idx], signed,
		); err != nil {
			return nil, fmt.Errorf("verify signed virtual packet %d: %w",
				idx, err)
		}
		active[idx] = cloneBytes(signed)
	}

	funding, err := lowLevelCustomAnchorFunding(p.request.Funding)
	if err != nil {
		return nil, err
	}
	commitRequest := &CommitVirtualPsbtsRequest{
		AnchorPsbt:   cloneBytes(p.anchorPSBT),
		VirtualPsbts: active,
		PassiveAssetPsbts: cloneByteSlices(
			p.passiveVirtualPSBTs,
		),
		Funding: funding,
	}
	response, err := advancedClient.CommitVirtualPsbtsWithRequest(
		ctx, commitRequest,
	)
	if err != nil {
		return nil, &CustomAnchorCommitAttemptError{
			Err: fmt.Errorf("commit custom anchor virtual packets: %w",
				err),
			FundingLock: customAnchorFundingLockMetadata(
				p.request.Funding,
			),
			OutcomeUnknown: customAnchorCommitOutcomeUnknown(err),
		}
	}
	if response == nil {
		return nil, &CustomAnchorCommitAttemptError{
			Err: fmt.Errorf("commit custom anchor returned no response"),
			FundingLock: customAnchorFundingLockMetadata(
				p.request.Funding,
			),
			OutcomeUnknown: true,
		}
	}
	locked := make([]CustomAnchorLockedUTXO, len(response.LockedUTXOs))
	for idx := range response.LockedUTXOs {
		locked[idx] = CustomAnchorLockedUTXO{
			Outpoint: response.LockedUTXOs[idx],
		}
	}
	defer func() {
		if err == nil {
			return
		}
		err = &CustomAnchorCommitResponseError{
			Err:         err,
			LockedUTXOs: append([]CustomAnchorLockedUTXO(nil), locked...),
		}
	}()
	if len(response.VirtualPsbts) != len(active) ||
		len(response.PassiveAssetPsbts) != len(p.passiveVirtualPSBTs) {

		return nil, fmt.Errorf("commit custom anchor changed virtual packet " +
			"counts")
	}

	preCommit, err := decodeAnchorPSBT(p.anchorPSBT)
	if err != nil {
		return nil, err
	}
	committedAnchor, err := decodeAnchorPSBT(response.AnchorPsbt)
	if err != nil {
		return nil, fmt.Errorf("decode committed anchor PSBT: %w", err)
	}
	if err := verifyCommittedAnchorShape(
		p.request, preCommit, committedAnchor,
		response.ChangeOutputIndex,
	); err != nil {
		return nil, err
	}
	if err := validateTxIDStableAnchorInputs(committedAnchor); err != nil {
		return nil, fmt.Errorf("validate committed anchor inputs: %w", err)
	}
	if err := validateCustomAnchorBitcoinTransaction(
		committedAnchor, true,
	); err != nil {
		return nil, fmt.Errorf("validate committed anchor transaction: %w", err)
	}
	actualFee, err := customAnchorTransactionFee(committedAnchor)
	if err != nil {
		return nil, fmt.Errorf("calculate committed anchor fee: %w", err)
	}
	if p.request.Funding.Mode == CustomAnchorFundingWalletFunded &&
		actualFee > p.request.Funding.WalletFunded.MaxFeeSat {

		return nil, fmt.Errorf("committed anchor fee %d exceeds maximum %d",
			actualFee, p.request.Funding.WalletFunded.MaxFeeSat)
	}
	backendManaged := addedAnchorInputIndices(preCommit, committedAnchor)
	if err := validateCustomAnchorLockInventory(
		p.request.Funding, committedAnchor, backendManaged, locked,
	); err != nil {
		return nil, err
	}

	activePackets, err := decodeVirtualPackets(
		"committed active", response.VirtualPsbts,
	)
	if err != nil {
		return nil, err
	}
	passivePackets, err := decodeVirtualPackets(
		"committed passive", response.PassiveAssetPsbts,
	)
	if err != nil {
		return nil, err
	}
	expectedActivePackets, err := decodeVirtualPackets(
		"submitted active", active,
	)
	if err != nil {
		return nil, err
	}
	expectedPassivePackets, err := decodeVirtualPackets(
		"submitted passive", p.passiveVirtualPSBTs,
	)
	if err != nil {
		return nil, err
	}
	expectedAllPackets := append(
		append([]*tappsbt.VPacket(nil), expectedActivePackets...),
		expectedPassivePackets...,
	)
	expectedOutputCommitments, err := tapsend.CreateOutputCommitments(
		expectedAllPackets, tapsend.WithSpenderLeaves(),
	)
	if err != nil {
		return nil, fmt.Errorf("derive expected committed virtual packets: %w",
			err)
	}
	if err := verifyCommittedVirtualPackets(
		"active", expectedActivePackets, activePackets,
	); err != nil {
		return nil, err
	}
	if err := verifyCommittedVirtualPackets(
		"passive", expectedPassivePackets, passivePackets,
	); err != nil {
		return nil, err
	}
	allPackets := append(
		append([]*tappsbt.VPacket(nil), activePackets...),
		passivePackets...,
	)
	if err := verifyCommittedProofSuffixes(
		committedAnchor, expectedAllPackets, allPackets,
		expectedOutputCommitments,
	); err != nil {
		return nil, err
	}
	if err := tapsend.ValidateAnchorInputs(
		committedAnchor, allPackets, nil,
	); err != nil {
		return nil, fmt.Errorf("verify committed anchor inputs: %w", err)
	}
	if err := tapsend.ValidateAnchorOutputs(
		committedAnchor, allPackets, false,
	); err != nil {
		return nil, fmt.Errorf("verify committed anchor outputs: %w", err)
	}

	planID, err := p.ID()
	if err != nil {
		return nil, err
	}
	inputs, err := p.committedInputSummaries(
		committedAnchor, activePackets, passivePackets,
	)
	if err != nil {
		return nil, err
	}
	outputs, proofUpdates, err := p.committedOutputSummaries(
		committedAnchor, activePackets, passivePackets,
	)
	if err != nil {
		return nil, err
	}

	packageSnapshot := &CustomAnchorTransferPackage{
		PlanID:              planID,
		AnchorPsbt:          cloneBytes(response.AnchorPsbt),
		ActiveVirtualPsbts:  cloneByteSlices(response.VirtualPsbts),
		PassiveVirtualPsbts: cloneByteSlices(response.PassiveAssetPsbts),
		ChangeOutputIndex:   response.ChangeOutputIndex,
		LockedUTXOs:         locked,
		FundingLock:         customAnchorFundingLockMetadata(p.request.Funding),
		Funding: customAnchorFundingSummary(
			p.request.Funding, actualFee,
		),
		Inputs:       inputs,
		Outputs:      outputs,
		ProofUpdates: proofUpdates,
		SigningPlans: cloneCustomAnchorSigningPlans(
			p.request.SigningPlans,
		),
		BackendManagedInputIndices: backendManaged,
		Publish:                    opts.Publish,
	}

	sealed, err := packageSnapshot.Seal()
	if err != nil {
		return nil, fmt.Errorf("seal committed custom anchor package: %w",
			err)
	}

	return sealed, nil
}

func customAnchorFundingSummary(funding CustomAnchorFundingPlan,
	actualFee uint64) CustomAnchorFundingSummary {

	summary := CustomAnchorFundingSummary{
		Mode:         funding.Mode,
		ActualFeeSat: actualFee,
	}
	if funding.WalletFunded != nil {
		summary.MaxFeeSat = funding.WalletFunded.MaxFeeSat
	}
	if funding.ExternalP2AFeeBump != nil {
		index := funding.ExternalP2AFeeBump.P2AOutputIndex
		summary.P2AOutputIndex = &index
	}

	return summary
}

func customAnchorCommitOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {

		return true
	}

	statusCode := status.Code(err)
	switch statusCode {
	case codes.InvalidArgument, codes.FailedPrecondition,
		codes.PermissionDenied, codes.Unauthenticated, codes.NotFound,
		codes.Unimplemented:

		// These are explicit backend rejections and therefore did not lose
		// a successful commit response in transit.
		return false

	default:
		// AlreadyExists can report a prior successful commit whose response
		// was lost. Unknown local errors and transient transport codes are
		// equally ambiguous because the backend may already hold wallet locks.
		return true
	}
}

func customAnchorFundingLockMetadata(
	funding CustomAnchorFundingPlan) CustomAnchorFundingLockMetadata {

	if funding.Mode != CustomAnchorFundingWalletFunded ||
		funding.WalletFunded == nil {

		return CustomAnchorFundingLockMetadata{}
	}

	return CustomAnchorFundingLockMetadata{
		CustomLockID: cloneBytes(funding.WalletFunded.CustomLockID),
		LockExpirationSeconds: funding.WalletFunded.
			LockExpirationSeconds,
	}
}

func validateCustomAnchorLockInventory(funding CustomAnchorFundingPlan,
	anchor *psbt.Packet, backendManaged []uint32,
	locked []CustomAnchorLockedUTXO) error {

	if funding.Mode != CustomAnchorFundingWalletFunded {
		if len(locked) != 0 {
			return fmt.Errorf("skip-funding commit returned locked UTXOs")
		}

		return nil
	}

	expected := make(map[Outpoint]struct{}, len(backendManaged))
	for _, inputIndex := range backendManaged {
		if inputIndex >= uint32(len(anchor.UnsignedTx.TxIn)) {
			return fmt.Errorf("backend-managed input %d is out of range",
				inputIndex)
		}
		outpoint := outpointFromWire(
			anchor.UnsignedTx.TxIn[inputIndex].PreviousOutPoint,
		)
		expected[outpoint] = struct{}{}
	}
	actual := make(map[Outpoint]struct{}, len(locked))
	for idx := range locked {
		outpoint := locked[idx].Outpoint
		if _, ok := actual[outpoint]; ok {
			return fmt.Errorf("locked UTXO %d is duplicated", idx)
		}
		actual[outpoint] = struct{}{}
	}
	if len(expected) != len(actual) {
		return fmt.Errorf("locked UTXOs do not cover backend-funded inputs")
	}
	for outpoint := range expected {
		if _, ok := actual[outpoint]; !ok {
			return fmt.Errorf("locked UTXOs do not cover backend-funded inputs")
		}
	}

	return nil
}

// PublishCustomAnchorTransfer verifies that the final PSBT has the exact
// committed unsigned transaction and commitments, then publishes/logs it using
// the metadata sealed into the package.
func (s *Wallet) PublishCustomAnchorTransfer(ctx context.Context,
	packageSnapshot *CustomAnchorTransferPackage,
	finalAnchorPSBT []byte) (*AssetPacket, error) {

	if s == nil || s.client == nil {
		return nil, fmt.Errorf("wallet has no client")
	}
	if err := packageSnapshot.Validate(); err != nil {
		return nil, fmt.Errorf("validate custom anchor package: %w", err)
	}
	if err := packageSnapshot.rejectUnconfirmedProofPaths(); err != nil {
		return nil, err
	}
	if len(finalAnchorPSBT) == 0 {
		return nil, fmt.Errorf("final anchor PSBT is required")
	}

	finalizedPackage, err := packageSnapshot.WithFinalAnchorPSBT(
		finalAnchorPSBT,
	)
	if err != nil {
		return nil, fmt.Errorf("verify final anchor PSBT: %w", err)
	}
	finalAnchorPSBT = finalizedPackage.AnchorPsbt
	finalAnchor, err := decodeAnchorPSBT(finalAnchorPSBT)
	if err != nil {
		return nil, fmt.Errorf("decode finalized anchor PSBT: %w", err)
	}
	if err := validateCustomAnchorBitcoinTransaction(
		finalAnchor, true,
	); err != nil {
		return nil, fmt.Errorf("validate final anchor transaction: %w", err)
	}
	active, err := decodeVirtualPackets(
		"active", packageSnapshot.ActiveVirtualPsbts,
	)
	if err != nil {
		return nil, err
	}
	passive, err := decodeVirtualPackets(
		"passive", packageSnapshot.PassiveVirtualPsbts,
	)
	if err != nil {
		return nil, err
	}
	allPackets := append(
		append([]*tappsbt.VPacket(nil), active...), passive...,
	)
	if err := tapsend.ValidateAnchorInputs(
		finalAnchor, allPackets, nil,
	); err != nil {
		return nil, fmt.Errorf("verify final anchor inputs: %w", err)
	}
	if err := tapsend.ValidateAnchorOutputs(
		finalAnchor, allPackets, false,
	); err != nil {
		return nil, fmt.Errorf("verify final anchor outputs: %w", err)
	}

	if packageSnapshot.Publish.SkipAnchorTxBroadcast {
		if err := requireCustomAnchorCapability(
			ctx, s.client, CustomAnchorCapabilitySkipBroadcast,
		); err != nil {
			return nil, err
		}
	}
	advancedClient, err := requireCustomAnchorWalletKitClient(
		s.client, CustomAnchorCapabilityCommit,
	)
	if err != nil {
		return nil, err
	}

	result, err := advancedClient.PublishAndLogTransferWithRequest(
		ctx, &PublishAndLogTransferRequest{
			AnchorPsbt: cloneBytes(finalAnchorPSBT),
			VirtualPsbts: cloneByteSlices(
				packageSnapshot.ActiveVirtualPsbts,
			),
			PassiveAssetPsbts: cloneByteSlices(
				packageSnapshot.PassiveVirtualPsbts,
			),
			ChangeOutputIndex: packageSnapshot.ChangeOutputIndex,
			LockedUTXOs: packageLockedOutpoints(
				packageSnapshot.LockedUTXOs,
			),
			SkipAnchorTxBroadcast: packageSnapshot.Publish.
				SkipAnchorTxBroadcast,
			Label: packageSnapshot.Publish.Label,
		},
	)
	if err != nil {
		return nil, &CustomAnchorPublishAttemptError{
			Err:            fmt.Errorf("publish custom anchor transfer: %w", err),
			OutcomeUnknown: customAnchorPublishOutcomeUnknown(err),
		}
	}
	if result == nil || len(result.AnchorTransaction) == 0 {
		return nil, &CustomAnchorPublishAttemptError{
			Err: fmt.Errorf("publish custom anchor returned no usable " +
				"response"),
			OutcomeUnknown: true,
		}
	}

	finalTx, err := psbt.Extract(finalAnchor)
	if err != nil {
		return nil, fmt.Errorf("extract final anchor transaction: %w", err)
	}
	var expectedAnchorTx bytes.Buffer
	if err := finalTx.Serialize(&expectedAnchorTx); err != nil {
		return nil, fmt.Errorf("serialize final anchor transaction: %w", err)
	}
	if !bytes.Equal(result.AnchorTransaction, expectedAnchorTx.Bytes()) ||
		!equalCustomAnchorPacketSlices(
			result.VirtualTransactions,
			packageSnapshot.ActiveVirtualPsbts,
		) || !equalCustomAnchorPacketSlices(
		result.PassiveAssetTransactions,
		packageSnapshot.PassiveVirtualPsbts,
	) {

		return nil, &CustomAnchorPublishAttemptError{
			Err: fmt.Errorf("publish custom anchor response does not match " +
				"the finalized transfer"),
			OutcomeUnknown: true,
		}
	}

	return result, nil
}

func equalCustomAnchorPacketSlices(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for idx := range left {
		if !bytes.Equal(left[idx], right[idx]) {
			return false
		}
	}

	return true
}

func customAnchorPublishOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {

		return true
	}

	switch status.Code(err) {
	case codes.InvalidArgument, codes.FailedPrecondition,
		codes.PermissionDenied, codes.Unauthenticated, codes.NotFound,
		codes.Unimplemented:

		return false

	default:
		// AlreadyExists can mean a prior publish attempt succeeded but its
		// response was lost. Transient and unknown transport failures are
		// equally ambiguous until the backend exposes status reconciliation.
		return true
	}
}

func requireCustomAnchorWalletKitClient(client Client,
	capability CustomAnchorCapability) (CustomAnchorWalletKitClient, error) {

	advanced, ok := client.(CustomAnchorWalletKitClient)
	if ok {
		return advanced, nil
	}

	return nil, &UnsupportedCustomAnchorCapabilityError{
		BackendMode: CustomAnchorBackendUnknown,
		Capability:  capability,
		Status:      CustomAnchorCapabilityUnknown,
	}
}

func requireCustomAnchorCommitCapabilities(ctx context.Context, client Client,
	funding CustomAnchorFundingPlan,
	publish CustomAnchorPublishMetadata) error {

	required := []CustomAnchorCapability{
		CustomAnchorCapabilityCommit,
		CustomAnchorCapabilityCallerAnchorPSBT,
	}
	if funding.Mode == CustomAnchorFundingWalletFunded {
		required = append(required, CustomAnchorCapabilityWalletFunding)
		if funding.WalletFunded.ChangeOutput.Mode ==
			AnchorChangeOutputNoNew {

			required = append(
				required, CustomAnchorCapabilityNoChangeOutput,
			)
		}
	} else {
		required = append(required, CustomAnchorCapabilitySkipFunding)
	}
	if publish.SkipAnchorTxBroadcast {
		required = append(required, CustomAnchorCapabilitySkipBroadcast)
	}

	for _, capability := range required {
		if err := requireCustomAnchorCapability(
			ctx, client, capability,
		); err != nil {
			return err
		}
	}

	return nil
}

func requireCustomAnchorCapability(ctx context.Context, client Client,
	capability CustomAnchorCapability) error {

	provider, ok := client.(CustomAnchorCapabilityProvider)
	if !ok {
		return (&CustomAnchorCapabilities{}).Require(capability)
	}
	caps, err := provider.CustomAnchorCapabilities(ctx)
	if err != nil {
		return fmt.Errorf("load custom anchor capabilities: %w", err)
	}
	if caps == nil {
		caps = &CustomAnchorCapabilities{}
	}

	return caps.Require(capability)
}

func lowLevelCustomAnchorFunding(plan CustomAnchorFundingPlan) (
	AnchorFundingPlan, error) {

	switch plan.Mode {
	case CustomAnchorFundingWalletFunded:
		wallet := plan.WalletFunded
		return AnchorFundingPlan{
			ChangeOutput:          wallet.ChangeOutput,
			Fee:                   wallet.Fee,
			CustomLockID:          cloneBytes(wallet.CustomLockID),
			LockExpirationSeconds: wallet.LockExpirationSeconds,
		}, nil

	case CustomAnchorFundingCallerFundedExact,
		CustomAnchorFundingExternalP2AFeeBump:

		return AnchorFundingPlan{SkipFunding: true}, nil

	default:
		return AnchorFundingPlan{}, fmt.Errorf("unknown custom anchor "+
			"funding mode %d", plan.Mode)
	}
}

func verifyCommittedAnchorShape(req *CustomAnchorRequest, before,
	after *psbt.Packet, changeIndex int32) error {

	if !reflect.DeepEqual(before.XPubs, after.XPubs) ||
		!reflect.DeepEqual(before.Unknowns, after.Unknowns) {

		return fmt.Errorf("backend changed anchor global PSBT metadata")
	}
	if before.UnsignedTx.Version != after.UnsignedTx.Version ||
		before.UnsignedTx.LockTime != after.UnsignedTx.LockTime {

		return fmt.Errorf("backend changed anchor version or locktime")
	}
	if len(after.UnsignedTx.TxIn) < len(before.UnsignedTx.TxIn) {
		return fmt.Errorf("backend removed anchor inputs")
	}
	for idx := range before.UnsignedTx.TxIn {
		left := before.UnsignedTx.TxIn[idx]
		right := after.UnsignedTx.TxIn[idx]
		if left.PreviousOutPoint != right.PreviousOutPoint ||
			left.Sequence != right.Sequence {

			return fmt.Errorf("backend changed anchor input %d outpoint or "+
				"sequence", idx)
		}
		if !customAnchorInputMetadataCompatible(
			before.Inputs[idx], after.Inputs[idx],
		) {

			return fmt.Errorf("backend changed anchor input %d PSBT metadata",
				idx)
		}
	}

	skipFunding := req.Funding.Mode != CustomAnchorFundingWalletFunded
	var existingChangeIndex int32 = -1
	if skipFunding {
		if len(after.UnsignedTx.TxIn) != len(before.UnsignedTx.TxIn) {
			return fmt.Errorf("skip-funding commit changed anchor inputs")
		}
		if len(after.UnsignedTx.TxOut) != len(before.UnsignedTx.TxOut) {
			return fmt.Errorf("skip-funding commit changed anchor output count")
		}
		if changeIndex != -1 {
			return fmt.Errorf("skip-funding commit returned change output %d",
				changeIndex)
		}
	} else {
		if len(after.UnsignedTx.TxOut) < len(before.UnsignedTx.TxOut) {
			return fmt.Errorf("backend removed anchor outputs")
		}
		changePlan := req.Funding.WalletFunded.ChangeOutput
		switch changePlan.Mode {
		case AnchorChangeOutputAdd:
			added := len(after.UnsignedTx.TxOut) -
				len(before.UnsignedTx.TxOut)
			if added > 1 {
				return fmt.Errorf("backend added %d change outputs", added)
			}
			if added == 0 && changeIndex != -1 {
				return fmt.Errorf("backend returned unexpected existing "+
					"change output %d", changeIndex)
			}
			if added == 1 && changeIndex !=
				int32(len(before.UnsignedTx.TxOut)) {

				return fmt.Errorf("backend returned change output %d, want %d",
					changeIndex, len(before.UnsignedTx.TxOut))
			}

		case AnchorChangeOutputExisting:
			existingChangeIndex = changePlan.ExistingOutputIndex
			if existingChangeIndex < 0 || existingChangeIndex >=
				int32(len(before.UnsignedTx.TxOut)) {

				return fmt.Errorf("requested existing change output %d is "+
					"out of range", existingChangeIndex)
			}
			if len(after.UnsignedTx.TxOut) != len(before.UnsignedTx.TxOut) {
				return fmt.Errorf("existing-change funding added an output")
			}
			if changeIndex != existingChangeIndex {
				return fmt.Errorf("backend returned change output %d, want %d",
					changeIndex, existingChangeIndex)
			}

		case AnchorChangeOutputNoNew:
			if len(after.UnsignedTx.TxOut) != len(before.UnsignedTx.TxOut) {
				return fmt.Errorf("no-new-change funding added an output")
			}
			if changeIndex != -1 {
				return fmt.Errorf("no-new-change funding returned change "+
					"output %d", changeIndex)
			}

		default:
			return fmt.Errorf("unknown anchor change policy %d",
				changePlan.Mode)
		}
	}

	assetOutputs := make(map[uint32]struct{}, len(req.Outputs))
	for idx := range req.Outputs {
		assetOutputs[req.Outputs[idx].AnchorOutputIndex] = struct{}{}
	}
	for idx := range before.UnsignedTx.TxOut {
		left := before.UnsignedTx.TxOut[idx]
		right := after.UnsignedTx.TxOut[idx]
		_, isAssetOutput := assetOutputs[uint32(idx)]
		isExistingChange := existingChangeIndex == int32(idx)
		if isExistingChange && isAssetOutput {
			return fmt.Errorf("asset output %d cannot be used as BTC change",
				idx)
		}

		if isAssetOutput {
			if left.Value != right.Value {
				return fmt.Errorf("backend changed asset output %d value", idx)
			}
			if !customAnchorOutputMetadataEqual(
				before.Outputs[idx], after.Outputs[idx],
			) {

				return fmt.Errorf("backend changed asset output %d PSBT "+
					"metadata", idx)
			}
			continue
		}
		if !bytes.Equal(left.PkScript, right.PkScript) {
			return fmt.Errorf("backend changed BTC-only output %d script",
				idx)
		}
		if !isExistingChange && left.Value != right.Value {
			return fmt.Errorf("backend changed BTC-only output %d value",
				idx)
		}
		if !reflect.DeepEqual(before.Outputs[idx], after.Outputs[idx]) {
			return fmt.Errorf("backend changed BTC-only output %d PSBT "+
				"metadata", idx)
		}
	}
	if !skipFunding && req.Funding.WalletFunded.ChangeOutput.Mode ==
		AnchorChangeOutputAdd {

		for idx := len(before.UnsignedTx.TxOut); idx <
			len(after.UnsignedTx.TxOut); idx++ {
			if changeIndex != int32(idx) {
				return fmt.Errorf("backend added non-change output %d", idx)
			}
		}
	}

	return nil
}

// customAnchorInputMetadataCompatible permits the backend to enrich an
// existing input with key-origin records needed by the configured anchor
// signer. Every caller-provided derivation and every other PSBT field remains
// immutable. Key-origin order is not semantically significant.
func customAnchorInputMetadataCompatible(before, after psbt.PInput) bool {
	beforeBip32 := before.Bip32Derivation
	afterBip32 := after.Bip32Derivation
	beforeTaprootBip32 := before.TaprootBip32Derivation
	afterTaprootBip32 := after.TaprootBip32Derivation

	before.Bip32Derivation = nil
	after.Bip32Derivation = nil
	before.TaprootBip32Derivation = nil
	after.TaprootBip32Derivation = nil
	if !reflect.DeepEqual(before, after) {
		return false
	}

	return customAnchorDerivationsContain(
		afterBip32, beforeBip32,
	) && customAnchorDerivationsContain(
		afterTaprootBip32, beforeTaprootBip32,
	)
}

func customAnchorDerivationsContain[T any](superset,
	subset []*T) bool {

	for _, expected := range subset {
		found := false
		for _, actual := range superset {
			if reflect.DeepEqual(expected, actual) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

func customAnchorOutputMetadataEqual(before, after psbt.POutput) bool {
	before.Unknowns = filterCustomAnchorCommitmentFields(before.Unknowns)
	after.Unknowns = filterCustomAnchorCommitmentFields(after.Unknowns)

	return reflect.DeepEqual(before, after)
}

func filterCustomAnchorCommitmentFields(
	unknowns []*psbt.Unknown) []*psbt.Unknown {

	filtered := make([]*psbt.Unknown, 0, len(unknowns))
	for _, unknown := range unknowns {
		if unknown != nil && (bytes.Equal(
			unknown.Key, tappsbt.PsbtKeyTypeOutputTaprootMerkleRoot,
		) || bytes.Equal(
			unknown.Key, tappsbt.PsbtKeyTypeOutputAssetRoot,
		)) {

			continue
		}
		filtered = append(filtered, unknown)
	}

	return filtered
}

func (p *CustomAnchorPlan) committedInputSummaries(anchor *psbt.Packet,
	active, passive []*tappsbt.VPacket) (
	[]CustomAnchorAssetInputSummary, error) {

	result := make([]CustomAnchorAssetInputSummary, 0)
	collections := []struct {
		role    CustomAnchorPacketRole
		packets []*tappsbt.VPacket
	}{
		{CustomAnchorPacketRoleActive, active},
		{CustomAnchorPacketRolePassive, passive},
	}
	for _, collection := range collections {
		for packetIdx := range collection.packets {
			packet := collection.packets[packetIdx]
			for inputIdx := range packet.Inputs {
				vIn := packet.Inputs[inputIdx]
				inputAsset := vIn.Asset()
				if inputAsset == nil {
					return nil, fmt.Errorf("committed virtual input %d:%d has "+
						"no asset", packetIdx, inputIdx)
				}
				inputScriptKey, err := vIn.PrevID.ScriptKey.ToPubKey()
				if err != nil {
					return nil, fmt.Errorf("committed virtual input %d:%d "+
						"script key: %w", packetIdx, inputIdx, err)
				}
				inputScriptKeyBytes, err := ParsePubKey(
					inputScriptKey.SerializeCompressed(),
				)
				if err != nil {
					return nil, err
				}
				anchorIndex := findAnchorInput(anchor, vIn.PrevID.OutPoint)
				if anchorIndex < 0 {
					return nil, fmt.Errorf("committed virtual input %d:%d has "+
						"no anchor input", packetIdx, inputIdx)
				}

				var planned *CustomAnchorAssetInputSummary
				for idx := range p.inputs {
					candidate := &p.inputs[idx]
					if candidate.PacketRole == collection.role &&
						candidate.AnchorOutpoint == outpointFromWire(
							vIn.PrevID.OutPoint,
						) && candidate.IssuanceID == AssetID(vIn.PrevID.ID) &&
						candidate.ScriptKey == inputScriptKeyBytes &&
						candidate.Amount == inputAsset.Amount {

						planned = candidate
						break
					}
				}
				if planned == nil {
					return nil, fmt.Errorf("committed virtual input %d:%d has "+
						"no logical input", packetIdx, inputIdx)
				}

				summary := *planned
				summary.ProofSource.Blob = cloneBytes(
					planned.ProofSource.Blob,
				)
				summary.PacketRole = collection.role
				summary.PacketIndex = uint32(packetIdx)
				summary.VirtualInputIndex = uint32(inputIdx)
				summary.AnchorInputIndex = uint32(anchorIndex)
				result = append(result, summary)
			}
		}
	}

	return result, nil
}

func (p *CustomAnchorPlan) committedOutputSummaries(anchor *psbt.Packet,
	active, passive []*tappsbt.VPacket) ([]CustomAnchorAssetOutputSummary,
	[]CustomAnchorProofUpdate, error) {

	txID := anchor.UnsignedTx.TxHash()
	allPackets := append(
		append([]*tappsbt.VPacket(nil), active...), passive...,
	)
	// The committed packets already contain tapd's deterministic STXO alt
	// leaves. Reconstruct roots from those persisted leaves without adding a
	// second copy.
	commitments, err := tapsend.CreateOutputCommitments(
		allPackets, tapsend.WithNoSTXOProofs(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("derive committed output roots: %w", err)
	}
	outputs := make([]CustomAnchorAssetOutputSummary, 0, len(p.outputs))
	updates := make([]CustomAnchorProofUpdate, 0, len(p.outputs))
	for idx := range p.outputs {
		planned := p.outputs[idx]
		packets := active
		if planned.PacketRole == CustomAnchorPacketRolePassive {
			packets = passive
		} else if planned.PacketRole != CustomAnchorPacketRoleActive {
			return nil, nil, fmt.Errorf("planned output %q has unknown packet "+
				"role %d", planned.LogicalOutputID, planned.PacketRole)
		}
		if planned.PacketIndex >= uint32(len(packets)) {
			return nil, nil, fmt.Errorf("planned output %q packet index "+
				"is out of range", planned.LogicalOutputID)
		}
		packet := packets[planned.PacketIndex]
		if planned.VirtualOutput >= uint32(len(packet.Outputs)) {
			return nil, nil, fmt.Errorf("planned output %q virtual index "+
				"is out of range", planned.LogicalOutputID)
		}
		vOut := packet.Outputs[planned.VirtualOutput]
		if vOut.AnchorOutputIndex != planned.AnchorOutput ||
			vOut.Amount != planned.Amount ||
			vOut.ProofSuffix == nil {

			return nil, nil, fmt.Errorf("committed output %q does not "+
				"match its inspected mapping", planned.LogicalOutputID)
		}
		if planned.AnchorOutput >= uint32(len(anchor.UnsignedTx.TxOut)) {
			return nil, nil, fmt.Errorf("committed output %q anchor index "+
				"is out of range", planned.LogicalOutputID)
		}

		anchorOutpoint := Outpoint{
			Txid:  [32]byte(txID),
			Index: planned.AnchorOutput,
		}
		anchorValue := anchor.UnsignedTx.TxOut[planned.AnchorOutput].Value
		if anchorValue < 0 || uint64(anchorValue) != planned.AnchorValueSat {
			return nil, nil, fmt.Errorf("committed output %q anchor value "+
				"changed", planned.LogicalOutputID)
		}
		taprootAssetRoot, taprootMerkleRoot, err :=
			deriveCustomAnchorOutputRoots(vOut, commitments)
		if err != nil {
			return nil, nil, fmt.Errorf("derive output %q commitment roots: %w",
				planned.LogicalOutputID, err)
		}
		proofBlob, err := encodeTransitionProof(vOut.ProofSuffix)
		if err != nil {
			return nil, nil, fmt.Errorf("encode output %q proof suffix: %w",
				planned.LogicalOutputID, err)
		}

		outputs = append(outputs, CustomAnchorAssetOutputSummary{
			LogicalOutputID:    planned.LogicalOutputID,
			LogicalOutputIndex: planned.RequestIndex,
			PacketRole:         planned.PacketRole,
			PacketIndex:        planned.PacketIndex,
			VirtualOutputIndex: planned.VirtualOutput,
			AnchorOutputIndex:  planned.AnchorOutput,
			AssetRef:           planned.AssetRef,
			IssuanceID:         planned.IssuanceID,
			AssetType:          planned.AssetType,
			AnchorOutpoint:     anchorOutpoint,
			AnchorValueSat:     anchorValue,
			TaprootAssetRoot:   taprootAssetRoot,
			TaprootMerkleRoot:  taprootMerkleRoot,
			ScriptKey:          planned.ScriptKey,
			Amount:             planned.Amount,
			ScriptMode:         planned.ScriptMode,
			ProofDelivery: CustomAssetProofDelivery{
				RecipientID:    planned.ProofDelivery.RecipientID,
				CourierAddress: planned.ProofDelivery.CourierAddress,
				OpaqueMetadata: cloneBytes(
					planned.ProofDelivery.OpaqueMetadata,
				),
			},
			OPTrueSpend: planned.OPTrueSpend.Clone(),
		})
		updates = append(updates, CustomAnchorProofUpdate{
			LogicalOutputID:    planned.LogicalOutputID,
			LogicalOutputIndex: planned.RequestIndex,
			PacketRole:         planned.PacketRole,
			PacketIndex:        planned.PacketIndex,
			VirtualOutputIndex: planned.VirtualOutput,
			AnchorOutputIndex:  planned.AnchorOutput,
			AssetRef:           planned.AssetRef,
			IssuanceID:         planned.IssuanceID,
			ScriptKey:          planned.ScriptKey,
			AnchorOutpoint:     anchorOutpoint,
			ProofBlob:          proofBlob,
			ProofDelivery: CustomAssetProofDelivery{
				RecipientID:    planned.ProofDelivery.RecipientID,
				CourierAddress: planned.ProofDelivery.CourierAddress,
				OpaqueMetadata: cloneBytes(
					planned.ProofDelivery.OpaqueMetadata,
				),
			},
		})
	}

	return outputs, updates, nil
}

func decodeVirtualPackets(label string, encoded [][]byte) (
	[]*tappsbt.VPacket, error) {

	packets := make([]*tappsbt.VPacket, len(encoded))
	for idx := range encoded {
		packet, err := tappsbt.Decode(encoded[idx])
		if err != nil {
			return nil, fmt.Errorf("decode %s virtual packet %d: %w",
				label, idx, err)
		}
		packets[idx] = packet
	}
	return packets, nil
}

func verifyBackendSignedVirtualPacket(unsignedBytes, signedBytes []byte) error {
	unsignedPacket, err := tappsbt.Decode(unsignedBytes)
	if err != nil {
		return fmt.Errorf("decode unsigned packet: %w", err)
	}
	signedPacket, err := tappsbt.Decode(signedBytes)
	if err != nil {
		return fmt.Errorf("decode signed packet: %w", err)
	}
	if err := validateCustomAssetPacketWitnesses(signedPacket); err != nil {
		return err
	}

	normalizeVirtualPacketSigningFields(unsignedPacket)
	normalizeVirtualPacketSigningFields(signedPacket)
	unsignedCanonical, err := tappsbt.Encode(unsignedPacket)
	if err != nil {
		return err
	}
	signedCanonical, err := tappsbt.Encode(signedPacket)
	if err != nil {
		return err
	}
	if !bytes.Equal(unsignedCanonical, signedCanonical) {
		return fmt.Errorf("backend signer changed non-signing packet fields")
	}

	return nil
}

func normalizeVirtualPacketSigningFields(packet *tappsbt.VPacket) {
	for _, input := range packet.Inputs {
		if input == nil {
			continue
		}
		input.Bip32Derivation = nil
		input.TaprootBip32Derivation = nil
	}
	for _, output := range packet.Outputs {
		if output == nil {
			continue
		}
		clearCustomAssetTxWitnesses(output.Asset, 0)
		clearCustomAssetTxWitnesses(output.SplitAsset, 0)
	}
}

func clearCustomAssetTxWitnesses(a *asset.Asset, depth int) {
	if a == nil || depth > 2 {
		return
	}
	for idx := range a.PrevWitnesses {
		a.PrevWitnesses[idx].TxWitness = nil
		if a.PrevWitnesses[idx].SplitCommitment != nil {
			clearCustomAssetTxWitnesses(
				&a.PrevWitnesses[idx].SplitCommitment.RootAsset,
				depth+1,
			)
		}
	}
}

func encodeTransitionProof(transition *proof.Proof) ([]byte, error) {
	var buf bytes.Buffer
	if err := transition.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func customAnchorTransitionProofsEqual(left, right *proof.Proof) (
	bool, error) {

	if left == nil || right == nil {
		return left == nil && right == nil, nil
	}
	leftBytes, err := encodeTransitionProof(left)
	if err != nil {
		return false, err
	}
	rightBytes, err := encodeTransitionProof(right)
	if err != nil {
		return false, err
	}
	leftNormalized, err := proof.Decode(leftBytes)
	if err != nil {
		return false, err
	}
	rightNormalized, err := proof.Decode(rightBytes)
	if err != nil {
		return false, err
	}

	if !customAnchorAltLeavesEqual(
		leftNormalized.AltLeaves, rightNormalized.AltLeaves,
	) {

		return false, nil
	}
	// Alt leaves are committed by key in an MS-SMT and their serialized
	// slice order is not semantically meaningful. tapsend can produce a
	// different order when several packets share one anchor output.
	leftNormalized.AltLeaves = nil
	rightNormalized.AltLeaves = nil

	return reflect.DeepEqual(leftNormalized, rightNormalized), nil
}

func customAnchorAltLeavesEqual(left, right []asset.AltLeaf[asset.Asset]) bool {
	if len(left) != len(right) {
		return false
	}

	rightByKey := make(map[[32]byte]*asset.Asset, len(right))
	for _, leaf := range asset.FromAltLeaves(right) {
		rightByKey[leaf.AssetCommitmentKey()] = leaf
	}
	for _, leaf := range asset.FromAltLeaves(left) {
		other, ok := rightByKey[leaf.AssetCommitmentKey()]
		if !ok || !leaf.DeepEqual(other) {
			return false
		}
	}

	return true
}

func verifyCommittedVirtualPackets(label string, expected,
	committed []*tappsbt.VPacket) error {

	if len(expected) != len(committed) {
		return fmt.Errorf("committed %s virtual packet count changed", label)
	}
	for idx := range expected {
		for outputIdx, output := range committed[idx].Outputs {
			if output == nil || output.ProofSuffix == nil {
				return fmt.Errorf("committed %s virtual packet %d output %d "+
					"has no proof suffix", label, idx, outputIdx)
			}

			if outputIdx >= len(expected[idx].Outputs) {
				continue
			}
			if !customAnchorAltLeavesEqual(
				expected[idx].Outputs[outputIdx].AltLeaves,
				output.AltLeaves,
			) {

				return fmt.Errorf("committed %s virtual packet %d output %d "+
					"alternate leaves changed", label, idx, outputIdx)
			}
		}

		if len(expected[idx].Inputs) != len(committed[idx].Inputs) {
			return fmt.Errorf("committed %s virtual packet %d input count "+
				"changed", label, idx)
		}
		for inputIdx := range expected[idx].Inputs {
			equal, err := customAnchorTransitionProofsEqual(
				expected[idx].Inputs[inputIdx].Proof,
				committed[idx].Inputs[inputIdx].Proof,
			)
			if err != nil {
				return fmt.Errorf("compare %s virtual packet %d input %d "+
					"proof: %w", label, idx, inputIdx, err)
			}
			if !equal {
				return fmt.Errorf("committed %s virtual packet %d input %d "+
					"proof changed", label, idx, inputIdx)
			}
		}

		expectedBytes, err := encodeVirtualPacketWithoutProofSuffixes(
			expected[idx],
		)
		if err != nil {
			return fmt.Errorf("encode submitted %s virtual packet %d: %w",
				label, idx, err)
		}
		committedBytes, err := encodeVirtualPacketWithoutProofSuffixes(
			committed[idx],
		)
		if err != nil {
			return fmt.Errorf("encode committed %s virtual packet %d: %w",
				label, idx, err)
		}
		if !bytes.Equal(expectedBytes, committedBytes) {
			return fmt.Errorf("committed %s virtual packet %d changed fields "+
				"other than deterministic alternate leaves and proof "+
				"suffixes (%s)", label, idx,
				describeVirtualPacketDelta(
					expectedBytes, committedBytes,
				))
		}
	}

	return nil
}

func encodeVirtualPacketWithoutProofSuffixes(
	packet *tappsbt.VPacket) ([]byte, error) {

	if packet == nil {
		return nil, fmt.Errorf("nil virtual packet")
	}

	// An input proof is a transition proof and carries alternate leaves
	// of its own, so it is subject to the same reordering. It is
	// compared with the proof comparator instead.
	inputProofs := make([]*proof.Proof, len(packet.Inputs))
	for idx, input := range packet.Inputs {
		if input == nil {
			return nil, fmt.Errorf("nil virtual input %d", idx)
		}
		inputProofs[idx] = input.Proof
		input.Proof = nil
	}
	defer func() {
		for idx, input := range packet.Inputs {
			input.Proof = inputProofs[idx]
		}
	}()

	proofSuffixes := make([]*proof.Proof, len(packet.Outputs))
	altLeaves := make([][]asset.AltLeaf[asset.Asset], len(packet.Outputs))
	for idx, output := range packet.Outputs {
		if output == nil {
			return nil, fmt.Errorf("nil virtual output %d", idx)
		}
		proofSuffixes[idx] = output.ProofSuffix
		output.ProofSuffix = nil

		// Alt leaves are committed by key in an MS-SMT, so their
		// serialized order carries no meaning; tapsend emits a
		// different one when several packets share an anchor output.
		// They are compared by key separately.
		altLeaves[idx] = output.AltLeaves
		output.AltLeaves = nil
	}
	defer func() {
		for idx, output := range packet.Outputs {
			output.ProofSuffix = proofSuffixes[idx]
			output.AltLeaves = altLeaves[idx]
		}
	}()

	return tappsbt.Encode(packet)
}

func verifyCommittedProofSuffixes(anchor *psbt.Packet, expected,
	committed []*tappsbt.VPacket,
	outputCommitments tappsbt.OutputCommitments) error {

	for packetIdx := range expected {
		for outputIdx := range expected[packetIdx].Outputs {
			expectedSuffix, err := tapsend.CreateProofSuffix(
				anchor.UnsignedTx, anchor.Outputs, expected[packetIdx],
				outputCommitments, outputIdx, expected,
				proof.WithVersion(proof.TransitionV1),
			)
			if err != nil {
				return fmt.Errorf("recompute committed proof suffix %d:%d: %w",
					packetIdx, outputIdx, err)
			}
			equal, err := customAnchorTransitionProofsEqual(
				expectedSuffix,
				committed[packetIdx].Outputs[outputIdx].ProofSuffix,
			)
			if err != nil {
				return err
			}
			if !equal {
				return fmt.Errorf("committed virtual packet %d output %d "+
					"proof suffix mismatch", packetIdx, outputIdx)
			}
		}
	}

	return nil
}

func verifyFinalAnchorWitnesses(packet *psbt.Packet,
	finalTx *wire.MsgTx) error {

	prevOutFetcher, prevOuts, err := anchorPrevOuts(packet)
	if err != nil {
		return fmt.Errorf("load final anchor previous outputs: %w", err)
	}
	sigHashes := txscript.NewTxSigHashes(finalTx, prevOutFetcher)
	for idx := range finalTx.TxIn {
		engine, err := txscript.NewEngine(
			prevOuts[idx].PkScript, finalTx, idx,
			txscript.StandardVerifyFlags, nil, sigHashes,
			prevOuts[idx].Value, prevOutFetcher,
		)
		if err != nil {
			return fmt.Errorf("create final anchor input %d verifier: %w",
				idx, err)
		}
		if err := engine.Execute(); err != nil {
			return fmt.Errorf("verify final anchor input %d witness: %w",
				idx, err)
		}
	}

	return nil
}

func sanitizeFinalAnchorPSBT(committedBytes, finalBytes []byte) (
	[]byte, *psbt.Packet, error) {

	committed, err := decodeAnchorPSBT(committedBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("committed anchor PSBT: %w", err)
	}
	final, err := decodeAnchorPSBT(finalBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("final anchor PSBT: %w", err)
	}
	if err := compareUnsignedAnchorTransactions(
		committed.UnsignedTx, final.UnsignedTx,
	); err != nil {
		return nil, nil, err
	}
	if len(committed.Inputs) != len(final.Inputs) {
		return nil, nil, fmt.Errorf("anchor PSBT input map count changed")
	}

	// The finalizer is allowed to contribute only the consensus
	// authorization fields. Every prevout, sighash, derivation, script,
	// proprietary field, and output map is taken from the committed package.
	for idx := range committed.Inputs {
		committed.Inputs[idx].FinalScriptSig = cloneBytes(
			final.Inputs[idx].FinalScriptSig,
		)
		committed.Inputs[idx].FinalScriptWitness = cloneBytes(
			final.Inputs[idx].FinalScriptWitness,
		)
	}

	sanitized, err := serializeAnchorPSBT(committed)
	if err != nil {
		return nil, nil, err
	}

	return sanitized, committed, nil
}

func addedAnchorInputIndices(before, after *psbt.Packet) []uint32 {
	if before == nil || after == nil || before.UnsignedTx == nil ||
		after.UnsignedTx == nil || len(after.UnsignedTx.TxIn) <=
		len(before.UnsignedTx.TxIn) {

		return nil
	}
	result := make([]uint32, 0, len(after.UnsignedTx.TxIn)-
		len(before.UnsignedTx.TxIn))
	for idx := len(before.UnsignedTx.TxIn); idx <
		len(after.UnsignedTx.TxIn); idx++ {
		result = append(result, uint32(idx))
	}
	return result
}

func packageLockedOutpoints(
	locked []CustomAnchorLockedUTXO) []Outpoint {

	result := make([]Outpoint, len(locked))
	for idx := range locked {
		result[idx] = locked[idx].Outpoint
	}
	return result
}

func writePlanDigestBytes(buf *bytes.Buffer, value []byte) {
	_ = binary.Write(buf, binary.BigEndian, uint64(len(value)))
	_, _ = buf.Write(value)
}

// describeVirtualPacketDelta names the PSBT sections and key types that
// differ between two encoded virtual packets. A bare "the packet changed"
// is not actionable when the caller cannot see either side, so the
// comparison failure carries the offending keys with it.
func describeVirtualPacketDelta(expected, committed []byte) string {
	left, err := psbt.NewFromRawBytes(bytes.NewReader(expected), false)
	if err != nil {
		return fmt.Sprintf("undiagnosable: parse submitted: %v", err)
	}
	right, err := psbt.NewFromRawBytes(bytes.NewReader(committed), false)
	if err != nil {
		return fmt.Sprintf("undiagnosable: parse committed: %v", err)
	}

	var deltas []string
	record := func(section string, l, r []*psbt.Unknown) {
		for _, key := range unknownKeyDelta(l, r) {
			deltas = append(deltas, fmt.Sprintf(
				"%s key 0x%x", section, key,
			))
		}
	}

	record("global", left.Unknowns, right.Unknowns)
	if len(left.Inputs) != len(right.Inputs) {
		deltas = append(deltas, fmt.Sprintf("input count %d != %d",
			len(left.Inputs), len(right.Inputs)))
	} else {
		for idx := range left.Inputs {
			record(
				fmt.Sprintf("input %d", idx),
				left.Inputs[idx].Unknowns,
				right.Inputs[idx].Unknowns,
			)
		}
	}
	if len(left.Outputs) != len(right.Outputs) {
		deltas = append(deltas, fmt.Sprintf("output count %d != %d",
			len(left.Outputs), len(right.Outputs)))
	} else {
		for idx := range left.Outputs {
			record(
				fmt.Sprintf("output %d", idx),
				left.Outputs[idx].Unknowns,
				right.Outputs[idx].Unknowns,
			)
		}
	}

	if len(deltas) == 0 {
		return "differing bytes outside the PSBT key-value sections"
	}

	return strings.Join(deltas, ", ")
}

// unknownKeyDelta returns the keys whose values differ between two PSBT
// unknown-field sets, including keys present on only one side.
func unknownKeyDelta(left, right []*psbt.Unknown) [][]byte {
	index := func(fields []*psbt.Unknown) map[string][]byte {
		byKey := make(map[string][]byte, len(fields))
		for _, field := range fields {
			byKey[string(field.Key)] = field.Value
		}

		return byKey
	}
	leftByKey, rightByKey := index(left), index(right)

	var keys [][]byte
	for _, field := range left {
		other, ok := rightByKey[string(field.Key)]
		if !ok || !bytes.Equal(field.Value, other) {
			keys = append(keys, field.Key)
		}
	}
	for _, field := range right {
		if _, ok := leftByKey[string(field.Key)]; !ok {
			keys = append(keys, field.Key)
		}
	}

	return keys
}
