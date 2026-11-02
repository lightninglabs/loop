package tapsdk

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightninglabs/taproot-assets/vm"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCustomAnchorCommitCapabilitiesFailClosed(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	plan, err := NewWallet(fixture.client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.NoError(t, err)

	_, err = plan.Commit(context.Background())
	require.Error(t, err)
	var capabilityErr *UnsupportedCustomAnchorCapabilityError
	require.ErrorAs(t, err, &capabilityErr)
	require.Equal(t, CustomAnchorCapabilityCommit, capabilityErr.Capability)
	require.Equal(t, CustomAnchorCapabilityUnknown, capabilityErr.Status)
}

func TestCustomAnchorP2ACommitRequiresSkipBroadcastCapability(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	var signCalls, commitCalls int
	fixture.client.signVirtual = func(_ context.Context, packet []byte) (
		[]byte, error) {

		signCalls++
		return packet, nil
	}
	fixture.client.commit = func(context.Context,
		*CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse, error) {

		commitCalls++
		return nil, errors.New("unexpected commit")
	}

	capabilities := DefaultTapdCustomAnchorCapabilities()
	capabilities.SkipBroadcast = CustomAnchorCapabilityUnknown
	client := &capableCustomAnchorBuilderTestClient{
		customAnchorBuilderTestClient: fixture.client,
		capabilities:                  &capabilities,
	}
	plan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.NoError(t, err)

	_, err = plan.Commit(context.Background())
	require.Error(t, err)
	var capabilityErr *UnsupportedCustomAnchorCapabilityError
	require.ErrorAs(t, err, &capabilityErr)
	require.Equal(t, CustomAnchorCapabilitySkipBroadcast,
		capabilityErr.Capability)
	require.Zero(t, signCalls)
	require.Zero(t, commitCalls)
}

func TestCustomAnchorLifecycleBackendSigningAndPublish(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	policyLeaf := txscript.NewBaseTapLeaf([]byte{txscript.OP_TRUE})
	fixture.request.Outputs[0].Anchor.Tapscript = CustomAnchorTapscriptPlan{
		TapLeaves: []TapLeaf{{Script: cloneBytes(policyLeaf.Script)}},
	}
	var (
		signCalls     int
		commitRequest *CommitVirtualPsbtsRequest
		publishReq    *PublishAndLogTransferRequest
	)
	fixture.client.signVirtual = func(_ context.Context, packet []byte) (
		[]byte, error) {

		signCalls++
		return customAnchorSignVirtualPacket(
			t, fixture.assetSpendKey, packet,
		), nil
	}
	fixture.client.commit = func(_ context.Context,
		req *CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse, error) {

		commitRequest = cloneCustomAnchorCommitRequest(req)
		return customAnchorTestCommitResponse(t, req), nil
	}
	var expectedResult *AssetPacket
	fixture.client.publish = func(_ context.Context,
		req *PublishAndLogTransferRequest) (*AssetPacket, error) {

		publishReq = cloneCustomAnchorPublishRequest(req)
		packet, err := decodeAnchorPSBT(req.AnchorPsbt)
		require.NoError(t, err)
		finalTx, err := psbt.Extract(packet)
		require.NoError(t, err)
		var anchorTx bytes.Buffer
		require.NoError(t, finalTx.Serialize(&anchorTx))
		expectedResult = &AssetPacket{
			AnchorTransaction: anchorTx.Bytes(),
			VirtualTransactions: cloneByteSlices(
				req.VirtualPsbts,
			),
			PassiveAssetTransactions: cloneByteSlices(
				req.PassiveAssetPsbts,
			),
		}
		return expectedResult, nil
	}

	capabilities := DefaultTapdCustomAnchorCapabilities()
	client := &capableCustomAnchorBuilderTestClient{
		customAnchorBuilderTestClient: fixture.client,
		capabilities:                  &capabilities,
	}
	wallet := NewWallet(client, NetworkRegtest)
	plan, err := wallet.NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.NoError(t, err)
	planID, err := plan.ID()
	require.NoError(t, err)

	publishMetadata := CustomAnchorPublishMetadata{
		SkipAnchorTxBroadcast: true,
		Label:                 "swapdk-round-42",
		ExternalBroadcast:     true,
	}
	packageSnapshot, err := plan.Commit(
		context.Background(),
		CustomAnchorCommitOptions{Publish: publishMetadata},
	)
	require.NoError(t, err)
	require.NoError(t, packageSnapshot.Validate())
	require.True(t, packageSnapshot.Publish.SkipAnchorTxBroadcast)
	require.True(t, packageSnapshot.Publish.ExternalBroadcast)
	require.Equal(t, 1, signCalls)
	require.NotNil(t, commitRequest)
	require.True(t, commitRequest.Funding.SkipFunding)
	require.Equal(t, plan.AnchorPSBT(), commitRequest.AnchorPsbt)
	require.Len(t, commitRequest.VirtualPsbts, 1)
	require.NotEqual(t, plan.ActiveVirtualPSBTs()[0],
		commitRequest.VirtualPsbts[0])
	require.Nil(t, commitRequest.PassiveAssetPsbts)

	require.Equal(t, planID, packageSnapshot.PlanID)
	require.Equal(t, int32(-1), packageSnapshot.ChangeOutputIndex)
	require.Empty(t, packageSnapshot.LockedUTXOs)
	require.Len(t, packageSnapshot.Inputs, 1)
	require.Len(t, packageSnapshot.Outputs, 1)
	require.Len(t, packageSnapshot.ProofUpdates, 1)
	require.NotEmpty(t, packageSnapshot.ProofUpdates[0].ProofBlob)
	transition, err := proof.Decode(
		packageSnapshot.ProofUpdates[0].ProofBlob,
	)
	require.NoError(t, err)
	require.Equal(t, proof.TransitionV1, transition.Version)
	require.Equal(t, publishMetadata, packageSnapshot.Publish)
	outputSummary := packageSnapshot.Outputs[0]
	require.NotZero(t, outputSummary.TaprootAssetRoot)
	require.NotZero(t, outputSummary.TaprootMerkleRoot)
	policyRoot := policyLeaf.TapHash()
	expectedRoot := asset.NewTapBranchHash(
		chainhash.Hash(outputSummary.TaprootAssetRoot), policyRoot,
	)
	require.Equal(t, Hash(expectedRoot), outputSummary.TaprootMerkleRoot)

	before, err := decodeAnchorPSBT(plan.AnchorPSBT())
	require.NoError(t, err)
	committed, err := decodeAnchorPSBT(packageSnapshot.AnchorPsbt)
	require.NoError(t, err)
	require.Equal(t, before.UnsignedTx.Version,
		committed.UnsignedTx.Version)
	require.Equal(t, before.UnsignedTx.LockTime,
		committed.UnsignedTx.LockTime)
	require.Equal(t, before.UnsignedTx.TxIn[0].Sequence,
		committed.UnsignedTx.TxIn[0].Sequence)
	require.Equal(t, before.UnsignedTx.TxIn[1].Sequence,
		committed.UnsignedTx.TxIn[1].Sequence)
	require.Equal(t, before.UnsignedTx.TxOut[0],
		committed.UnsignedTx.TxOut[0])
	require.Equal(t, before.UnsignedTx.TxOut[2],
		committed.UnsignedTx.TxOut[2])
	require.NotEqual(t, before.UnsignedTx.TxOut[1].PkScript,
		committed.UnsignedTx.TxOut[1].PkScript)
	require.Equal(t, before.UnsignedTx.TxOut[1].Value,
		committed.UnsignedTx.TxOut[1].Value)
	internalKey, err := btcec.ParsePubKey(
		fixture.request.Outputs[0].Anchor.InternalKey.PubKey[:],
	)
	require.NoError(t, err)
	outputKey := txscript.ComputeTaprootOutputKey(
		internalKey, outputSummary.TaprootMerkleRoot[:],
	)
	expectedPkScript, err := txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)
	require.Equal(t, expectedPkScript,
		committed.UnsignedTx.TxOut[outputSummary.AnchorOutputIndex].PkScript)

	_, err = wallet.PublishCustomAnchorTransfer(
		context.Background(), packageSnapshot,
		packageSnapshot.AnchorPsbt,
	)
	require.ErrorContains(t, err, "final anchor PSBT is not finalized")
	require.Nil(t, publishReq)

	finalAnchorPSBT := customAnchorFinalizePSBT(
		t, packageSnapshot.AnchorPsbt, fixture.btcInputKey,
		fixture.anchorInputKey,
	)
	result, err := wallet.PublishCustomAnchorTransfer(
		context.Background(), packageSnapshot, finalAnchorPSBT,
	)
	require.NoError(t, err)
	require.Same(t, expectedResult, result)
	require.NotNil(t, publishReq)
	require.Equal(t, finalAnchorPSBT, publishReq.AnchorPsbt)
	require.Equal(t, packageSnapshot.ActiveVirtualPsbts,
		publishReq.VirtualPsbts)
	require.Equal(t, packageSnapshot.PassiveVirtualPsbts,
		publishReq.PassiveAssetPsbts)
	require.Equal(t, packageSnapshot.ChangeOutputIndex,
		publishReq.ChangeOutputIndex)
	require.Empty(t, publishReq.LockedUTXOs)
	require.True(t, publishReq.SkipAnchorTxBroadcast)
	require.Equal(t, "swapdk-round-42", publishReq.Label)

	for _, unusable := range []*AssetPacket{nil, {}} {
		fixture.client.publish = func(context.Context,
			*PublishAndLogTransferRequest) (*AssetPacket, error) {

			return unusable, nil
		}
		_, err = wallet.PublishCustomAnchorTransfer(
			context.Background(), packageSnapshot, finalAnchorPSBT,
		)
		var attemptErr *CustomAnchorPublishAttemptError
		require.ErrorAs(t, err, &attemptErr)
		require.True(t, attemptErr.OutcomeUnknown)
	}

	for _, mismatch := range []*AssetPacket{
		{
			AnchorTransaction:        []byte{1},
			VirtualTransactions:      packageSnapshot.ActiveVirtualPsbts,
			PassiveAssetTransactions: packageSnapshot.PassiveVirtualPsbts,
		},
		{
			AnchorTransaction:        expectedResult.AnchorTransaction,
			VirtualTransactions:      [][]byte{{1}},
			PassiveAssetTransactions: packageSnapshot.PassiveVirtualPsbts,
		},
		{
			AnchorTransaction:   expectedResult.AnchorTransaction,
			VirtualTransactions: packageSnapshot.ActiveVirtualPsbts,
			PassiveAssetTransactions: [][]byte{
				{1},
			},
		},
	} {
		fixture.client.publish = func(context.Context,
			*PublishAndLogTransferRequest) (*AssetPacket, error) {

			return mismatch, nil
		}
		_, err = wallet.PublishCustomAnchorTransfer(
			context.Background(), packageSnapshot, finalAnchorPSBT,
		)
		var attemptErr *CustomAnchorPublishAttemptError
		require.ErrorAs(t, err, &attemptErr)
		require.True(t, attemptErr.OutcomeUnknown)
		require.ErrorContains(t, err, "does not match")
	}
}

func TestCustomAnchorCommitPersistsFundingLock(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	feeRate, err := NewFeeRateSatPerVByte(2)
	require.NoError(t, err)

	lockID := bytes.Repeat([]byte{4}, 32)
	fixture.request.Funding = CustomAnchorFundingPlan{
		Mode: CustomAnchorFundingWalletFunded,
		WalletFunded: &CustomAnchorWalletFunding{
			ChangeOutput: AnchorChangeOutput{
				Mode: AnchorChangeOutputAdd,
			},
			Fee: AnchorFee{
				Mode:    AnchorFeeSatPerVByte,
				FeeRate: feeRate,
			},
			MaxFeeSat:             10_000,
			CustomLockID:          bytes.Clone(lockID),
			LockExpirationSeconds: 900,
		},
	}
	fixture.client.signVirtual = func(_ context.Context, packet []byte) (
		[]byte, error) {

		return customAnchorSignVirtualPacket(
			t, fixture.assetSpendKey, packet,
		), nil
	}
	var commitRequest *CommitVirtualPsbtsRequest
	fixture.client.commit = func(_ context.Context,
		req *CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse, error) {

		commitRequest = cloneCustomAnchorCommitRequest(req)
		return customAnchorTestCommitResponse(t, req), nil
	}

	capabilities := DefaultTapdCustomAnchorCapabilities()
	client := &capableCustomAnchorBuilderTestClient{
		customAnchorBuilderTestClient: fixture.client,
		capabilities:                  &capabilities,
	}
	plan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.NoError(t, err)

	fixture.request.Funding.WalletFunded.CustomLockID[0] = 5
	packageSnapshot, err := plan.Commit(context.Background())
	require.NoError(t, err)
	require.NotNil(t, commitRequest)
	require.Equal(t, lockID, commitRequest.Funding.CustomLockID)
	require.Equal(
		t, uint64(900), commitRequest.Funding.LockExpirationSeconds,
	)
	require.Equal(t, lockID, packageSnapshot.FundingLock.CustomLockID)
	require.Equal(
		t, uint64(900),
		packageSnapshot.FundingLock.LockExpirationSeconds,
	)

	clone := packageSnapshot.Clone()
	packageSnapshot.FundingLock.CustomLockID[0] = 6
	require.Equal(t, byte(4), clone.FundingLock.CustomLockID[0])
	clone.FundingLock.CustomLockID[1] = 7
	require.Equal(
		t, byte(4), packageSnapshot.FundingLock.CustomLockID[1],
	)
}

func TestCustomAnchorCommitEnforcesWalletFeeCeiling(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	anchor := mustDecodeAnchorPSBT(t, fixture.request.AnchorPSBT)
	anchor.UnsignedTx.TxOut = anchor.UnsignedTx.TxOut[:2]
	anchor.Outputs = anchor.Outputs[:2]
	fixture.request.AnchorPSBT = mustSerializeAnchorPSBT(t, anchor)

	feeRate, err := NewFeeRateSatPerVByte(2)
	require.NoError(t, err)
	lockID := bytes.Repeat([]byte{7}, 32)
	fixture.request.Funding = CustomAnchorFundingPlan{
		Mode: CustomAnchorFundingWalletFunded,
		WalletFunded: &CustomAnchorWalletFunding{
			ChangeOutput: AnchorChangeOutput{
				Mode: AnchorChangeOutputAdd,
			},
			Fee: AnchorFee{
				Mode:    AnchorFeeSatPerVByte,
				FeeRate: feeRate,
			},
			MaxFeeSat:    100,
			CustomLockID: lockID,
		},
	}
	fixture.client.signVirtual = func(_ context.Context, packet []byte) (
		[]byte, error) {

		return customAnchorSignVirtualPacket(
			t, fixture.assetSpendKey, packet,
		), nil
	}
	fixture.client.commit = func(_ context.Context,
		req *CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse, error) {

		response := customAnchorTestCommitResponse(t, req)
		committed := mustDecodeAnchorPSBT(t, response.AnchorPsbt)
		backendKey := testPrivateKey(t, 111).PubKey()
		backendScript, err := txscript.PayToTaprootScript(
			txscript.ComputeTaprootKeyNoScript(backendKey),
		)
		require.NoError(t, err)
		backendOutpoint := wire.OutPoint{
			Hash:  chainhash.Hash{111},
			Index: 3,
		}
		committed.UnsignedTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: backendOutpoint,
		})
		committed.Inputs = append(committed.Inputs, psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    1_000,
				PkScript: backendScript,
			},
		})
		committed.UnsignedTx.AddTxOut(&wire.TxOut{
			Value:    899,
			PkScript: []byte{txscript.OP_TRUE},
		})
		committed.Outputs = append(committed.Outputs, psbt.POutput{})
		response.AnchorPsbt = mustSerializeAnchorPSBT(t, committed)
		response.ChangeOutputIndex = 2
		response.LockedUTXOs = []Outpoint{
			outpointFromWire(backendOutpoint),
		}

		return response, nil
	}

	capabilities := DefaultTapdCustomAnchorCapabilities()
	client := &capableCustomAnchorBuilderTestClient{
		customAnchorBuilderTestClient: fixture.client,
		capabilities:                  &capabilities,
	}
	plan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.NoError(t, err)

	_, err = plan.Commit(context.Background())
	require.ErrorContains(t, err, "committed anchor fee 101 exceeds maximum 100")
}

func TestCustomAnchorCommitAttemptOutcome(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	feeRate, err := NewFeeRateSatPerVByte(2)
	require.NoError(t, err)
	lockID := bytes.Repeat([]byte{8}, 32)
	fixture.request.Funding = CustomAnchorFundingPlan{
		Mode: CustomAnchorFundingWalletFunded,
		WalletFunded: &CustomAnchorWalletFunding{
			ChangeOutput: AnchorChangeOutput{
				Mode: AnchorChangeOutputAdd,
			},
			Fee: AnchorFee{
				Mode:    AnchorFeeSatPerVByte,
				FeeRate: feeRate,
			},
			MaxFeeSat:             10_000,
			CustomLockID:          bytes.Clone(lockID),
			LockExpirationSeconds: 1_200,
		},
	}
	fixture.client.signVirtual = func(_ context.Context, packet []byte) (
		[]byte, error) {

		return customAnchorSignVirtualPacket(
			t, fixture.assetSpendKey, packet,
		), nil
	}

	capabilities := DefaultTapdCustomAnchorCapabilities()
	client := &capableCustomAnchorBuilderTestClient{
		customAnchorBuilderTestClient: fixture.client,
		capabilities:                  &capabilities,
	}
	plan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.NoError(t, err)

	fixture.client.commit = func(_ context.Context,
		req *CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse, error) {

		// Model the backend completing commit and retaining its leases,
		// followed by loss of the response at the transport boundary.
		_ = customAnchorTestCommitResponse(t, req)
		return nil, status.Error(
			codes.DeadlineExceeded, "commit response was lost",
		)
	}

	_, err = plan.Commit(context.Background())
	require.Error(t, err)
	var attemptErr *CustomAnchorCommitAttemptError
	require.ErrorAs(t, err, &attemptErr)
	require.True(t, attemptErr.OutcomeUnknown)
	require.Equal(t, lockID, attemptErr.FundingLock.CustomLockID)
	require.Equal(
		t, uint64(1_200),
		attemptErr.FundingLock.LockExpirationSeconds,
	)
	attemptErr.FundingLock.CustomLockID[0]++
	require.Equal(t, byte(8), lockID[0])

	fixture.client.commit = func(_ context.Context,
		_ *CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse, error) {

		return nil, status.Error(
			codes.InvalidArgument, "request was rejected",
		)
	}
	_, err = plan.Commit(context.Background())
	require.ErrorAs(t, err, &attemptErr)
	require.False(t, attemptErr.OutcomeUnknown)

	fixture.client.commit = func(_ context.Context,
		_ *CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse, error) {

		return nil, nil
	}
	_, err = plan.Commit(context.Background())
	require.ErrorAs(t, err, &attemptErr)
	require.True(t, attemptErr.OutcomeUnknown)
	require.Equal(t, lockID, attemptErr.FundingLock.CustomLockID)
}

func TestCustomAnchorPublishOutcomeClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		unknown bool
	}{
		{
			name:    "lost response",
			err:     status.Error(codes.DeadlineExceeded, "lost"),
			unknown: true,
		},
		{
			name:    "duplicate may be prior success",
			err:     status.Error(codes.AlreadyExists, "duplicate"),
			unknown: true,
		},
		{
			name: "explicit rejection",
			err:  status.Error(codes.InvalidArgument, "rejected"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(
				t, test.unknown,
				customAnchorPublishOutcomeUnknown(test.err),
			)
		})
	}
}

func TestCustomAnchorCommitOutcomeClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		unknown bool
	}{
		{
			name:    "lost response",
			err:     status.Error(codes.DeadlineExceeded, "lost"),
			unknown: true,
		},
		{
			name:    "duplicate may be prior success",
			err:     status.Error(codes.AlreadyExists, "duplicate"),
			unknown: true,
		},
		{
			name: "explicit rejection",
			err:  status.Error(codes.InvalidArgument, "rejected"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(
				t, test.unknown,
				customAnchorCommitOutcomeUnknown(test.err),
			)
		})
	}
}

func TestValidateCustomAnchorLockInventory(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	anchor, err := decodeAnchorPSBT(fixture.request.AnchorPSBT)
	require.NoError(t, err)

	var backendHash chainhash.Hash
	backendHash[0] = 9
	backendOutpoint := wire.OutPoint{
		Hash:  backendHash,
		Index: 3,
	}
	anchor.UnsignedTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: backendOutpoint,
	})
	anchor.Inputs = append(anchor.Inputs, psbt.PInput{})
	locked := CustomAnchorLockedUTXO{
		Outpoint: outpointFromWire(backendOutpoint),
	}
	wrong := CustomAnchorLockedUTXO{
		Outpoint: Outpoint{Txid: [32]byte{8}, Index: 4},
	}
	callerFunding := CustomAnchorFundingPlan{
		Mode:              CustomAnchorFundingCallerFundedExact,
		CallerFundedExact: &CustomAnchorCallerFundedExact{},
	}
	walletFunding := CustomAnchorFundingPlan{
		Mode:         CustomAnchorFundingWalletFunded,
		WalletFunded: &CustomAnchorWalletFunding{},
	}

	tests := []struct {
		name           string
		funding        CustomAnchorFundingPlan
		backendManaged []uint32
		locked         []CustomAnchorLockedUTXO
		wantErr        string
	}{
		{
			name:    "skip funding without locks",
			funding: callerFunding,
		},
		{
			name:    "skip funding with lock",
			funding: callerFunding,
			locked:  []CustomAnchorLockedUTXO{locked},
			wantErr: "skip-funding commit returned locked UTXOs",
		},
		{
			name:           "wallet lock covers backend input",
			funding:        walletFunding,
			backendManaged: []uint32{2},
			locked:         []CustomAnchorLockedUTXO{locked},
		},
		{
			name:           "missing wallet lock",
			funding:        walletFunding,
			backendManaged: []uint32{2},
			wantErr:        testDoNotCoverBackendFundedInputs,
		},
		{
			name:           "wrong wallet lock",
			funding:        walletFunding,
			backendManaged: []uint32{2},
			locked:         []CustomAnchorLockedUTXO{wrong},
			wantErr:        testDoNotCoverBackendFundedInputs,
		},
		{
			name:           "duplicate wallet lock",
			funding:        walletFunding,
			backendManaged: []uint32{2},
			locked:         []CustomAnchorLockedUTXO{locked, locked},
			wantErr:        "locked UTXO 1 is duplicated",
		},
		{
			name:           "backend input out of range",
			funding:        walletFunding,
			backendManaged: []uint32{3},
			wantErr:        "backend-managed input 3 is out of range",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCustomAnchorLockInventory(
				test.funding, anchor, test.backendManaged,
				test.locked,
			)
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorCommitRejectsBackendResponseDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		mutateResponse func(*CommitVirtualPsbtsResponse)
		mutatePackets  func(*psbt.Packet, []*tappsbt.VPacket)
		wantErr        string
	}{
		{
			name: testVersion,
			mutateResponse: func(response *CommitVirtualPsbtsResponse) {
				customAnchorMutateResponsePSBT(
					t, response,
					func(packet *psbt.Packet) {
						packet.UnsignedTx.Version++
					},
				)
			},
			wantErr: "changed anchor version or locktime",
		},
		{
			name: testLocktime,
			mutateResponse: func(response *CommitVirtualPsbtsResponse) {
				customAnchorMutateResponsePSBT(
					t, response,
					func(packet *psbt.Packet) {
						packet.UnsignedTx.LockTime++
					},
				)
			},
			wantErr: "changed anchor version or locktime",
		},
		{
			name: "input sequence",
			mutateResponse: func(response *CommitVirtualPsbtsResponse) {
				customAnchorMutateResponsePSBT(
					t, response,
					func(packet *psbt.Packet) {
						packet.UnsignedTx.TxIn[0].Sequence++
					},
				)
			},
			wantErr: "changed anchor input 0 outpoint or sequence",
		},
		{
			name: "BTC-only output script",
			mutateResponse: func(response *CommitVirtualPsbtsResponse) {
				customAnchorMutateResponsePSBT(
					t, response,
					func(packet *psbt.Packet) {
						packet.UnsignedTx.TxOut[0].PkScript =
							[]byte{txscript.OP_FALSE}
					},
				)
			},
			wantErr: "changed BTC-only output 0 script",
		},
		{
			name: "BTC-only output value",
			mutateResponse: func(response *CommitVirtualPsbtsResponse) {
				customAnchorMutateResponsePSBT(
					t, response,
					func(packet *psbt.Packet) {
						packet.UnsignedTx.TxOut[0].Value++
					},
				)
			},
			wantErr: "changed BTC-only output 0 value",
		},
		{
			name: "P2A output",
			mutateResponse: func(response *CommitVirtualPsbtsResponse) {
				customAnchorMutateResponsePSBT(
					t, response,
					func(packet *psbt.Packet) {
						packet.UnsignedTx.TxOut[2].PkScript =
							[]byte{txscript.OP_TRUE}
					},
				)
			},
			wantErr: "changed BTC-only output 2 script",
		},
		{
			name: "skip-funding input addition",
			mutateResponse: func(response *CommitVirtualPsbtsResponse) {
				customAnchorMutateResponsePSBT(
					t, response,
					func(packet *psbt.Packet) {
						packet.UnsignedTx.AddTxIn(&wire.TxIn{})
						packet.Inputs = append(
							packet.Inputs, psbt.PInput{},
						)
					},
				)
			},
			wantErr: "skip-funding commit changed anchor inputs",
		},
		{
			name: "virtual packet count",
			mutateResponse: func(response *CommitVirtualPsbtsResponse) {
				response.VirtualPsbts = nil
			},
			wantErr: "changed virtual packet counts",
		},
		{
			name: "virtual output script key",
			mutatePackets: func(_ *psbt.Packet,
				active []*tappsbt.VPacket) {

				output := active[0].Outputs[0]
				rawKey := testPrivateKey(t, 101).PubKey()
				outputKey := txscript.ComputeTaprootKeyNoScript(
					rawKey,
				)
				scriptKey := asset.NewScriptKey(outputKey)
				output.ScriptKey = scriptKey
				output.Asset.ScriptKey = scriptKey
				if output.SplitAsset != nil {
					output.SplitAsset.ScriptKey = scriptKey
				}
			},
			wantErr: "committed active virtual packet 0 output 0 " +
				"alternate leaves changed",
		},
		{
			name: "virtual anchor internal key",
			mutatePackets: func(anchor *psbt.Packet,
				active []*tappsbt.VPacket) {

				output := active[0].Outputs[0]
				newKey := testPrivateKey(t, 102).PubKey()
				output.AnchorOutputInternalKey = newKey
				anchor.Outputs[output.AnchorOutputIndex].
					TaprootInternalKey =
					schnorr.SerializePubKey(newKey)
			},
			wantErr: "backend changed asset output 1 PSBT metadata",
		},
		{
			name: "transition proof version downgrade",
			mutateResponse: func(response *CommitVirtualPsbtsResponse) {
				packet, err := tappsbt.Decode(response.VirtualPsbts[0])
				require.NoError(t, err)
				require.NotNil(t, packet.Outputs[0].ProofSuffix)
				packet.Outputs[0].ProofSuffix.Version =
					proof.TransitionV0
				response.VirtualPsbts[0], err = tappsbt.Encode(packet)
				require.NoError(t, err)
			},
			wantErr: "proof suffix mismatch",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newCustomAnchorBuilderFixture(t)
			fixture.client.signVirtual = func(_ context.Context,
				packet []byte) ([]byte, error) {

				return customAnchorSignVirtualPacket(
					t, fixture.assetSpendKey, packet,
				), nil
			}
			fixture.client.commit = func(_ context.Context,
				req *CommitVirtualPsbtsRequest) (
				*CommitVirtualPsbtsResponse, error) {

				response := customAnchorTestCommitResponseWithMutation(
					t, req, test.mutatePackets,
				)
				if test.mutateResponse != nil {
					test.mutateResponse(response)
				}
				return response, nil
			}
			capabilities := DefaultTapdCustomAnchorCapabilities()
			client := &capableCustomAnchorBuilderTestClient{
				customAnchorBuilderTestClient: fixture.client,
				capabilities:                  &capabilities,
			}
			plan, err := NewWallet(client, NetworkRegtest).
				NewCustomAnchorTxBuilder().Build(
				context.Background(), fixture.request,
			)
			require.NoError(t, err)

			_, err = plan.Commit(context.Background())
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorInputMetadataCompatible(t *testing.T) {
	t.Parallel()

	keys := make([]*btcec.PublicKey, 3)
	for idx := range keys {
		keys[idx] = testPrivateKey(t, byte(110+idx)).PubKey()
	}
	newInput := func() psbt.PInput {
		return psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    1_000,
				PkScript: []byte{txscript.OP_TRUE},
			},
			SighashType: txscript.SigHashDefault,
			Bip32Derivation: []*psbt.Bip32Derivation{
				{
					PubKey:    keys[0].SerializeCompressed(),
					Bip32Path: []uint32{1, 2},
				},
				{
					PubKey:    keys[1].SerializeCompressed(),
					Bip32Path: []uint32{3, 4},
				},
			},
			TaprootBip32Derivation: []*psbt.TaprootBip32Derivation{
				{
					XOnlyPubKey: schnorr.SerializePubKey(keys[0]),
					Bip32Path:   []uint32{5, 6},
				},
			},
			TaprootInternalKey: schnorr.SerializePubKey(keys[0]),
		}
	}

	tests := []struct {
		name   string
		mutate func(*psbt.PInput)
		want   bool
	}{
		{
			name: "unchanged",
			want: true,
		},
		{
			name: "backend derivation enrichment",
			mutate: func(input *psbt.PInput) {
				input.Bip32Derivation = append(
					input.Bip32Derivation,
					&psbt.Bip32Derivation{
						PubKey: keys[2].SerializeCompressed(),
						Bip32Path: []uint32{
							7, 8,
						},
					},
				)
				input.TaprootBip32Derivation = append(
					input.TaprootBip32Derivation,
					&psbt.TaprootBip32Derivation{
						XOnlyPubKey: schnorr.SerializePubKey(
							keys[2],
						),
						Bip32Path: []uint32{9, 10},
					},
				)
			},
			want: true,
		},
		{
			name: "derivation reorder",
			mutate: func(input *psbt.PInput) {
				input.Bip32Derivation[0],
					input.Bip32Derivation[1] =
					input.Bip32Derivation[1],
					input.Bip32Derivation[0]
			},
			want: true,
		},
		{
			name: "caller derivation removed",
			mutate: func(input *psbt.PInput) {
				input.Bip32Derivation = input.Bip32Derivation[1:]
			},
		},
		{
			name: "caller derivation changed",
			mutate: func(input *psbt.PInput) {
				input.Bip32Derivation[0].Bip32Path[0]++
			},
		},
		{
			name: "taproot derivation removed",
			mutate: func(input *psbt.PInput) {
				input.TaprootBip32Derivation = nil
			},
		},
		{
			name: "non-derivation metadata changed",
			mutate: func(input *psbt.PInput) {
				input.SighashType = txscript.SigHashSingle
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := newInput()
			after := newInput()
			if test.mutate != nil {
				test.mutate(&after)
			}

			require.Equal(t, test.want,
				customAnchorInputMetadataCompatible(before, after))
		})
	}
}

func TestPublishCustomAnchorTransferRejectsUnsignedDrift(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	fixture.client.signVirtual = func(_ context.Context, packet []byte) (
		[]byte, error) {

		return customAnchorSignVirtualPacket(
			t, fixture.assetSpendKey, packet,
		), nil
	}
	fixture.client.commit = func(_ context.Context,
		req *CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse, error) {

		return customAnchorTestCommitResponse(t, req), nil
	}
	publishCalls := 0
	fixture.client.publish = func(context.Context,
		*PublishAndLogTransferRequest) (*AssetPacket, error) {

		publishCalls++
		return &AssetPacket{}, nil
	}
	capabilities := DefaultTapdCustomAnchorCapabilities()
	client := &capableCustomAnchorBuilderTestClient{
		customAnchorBuilderTestClient: fixture.client,
		capabilities:                  &capabilities,
	}
	wallet := NewWallet(client, NetworkRegtest)
	plan, err := wallet.NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.NoError(t, err)
	packageSnapshot, err := plan.Commit(context.Background())
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*psbt.Packet)
	}{
		{
			name: testVersion,
			mutate: func(packet *psbt.Packet) {
				packet.UnsignedTx.Version++
			},
		},
		{
			name: testLocktime,
			mutate: func(packet *psbt.Packet) {
				packet.UnsignedTx.LockTime++
			},
		},
		{
			name: "sequence",
			mutate: func(packet *psbt.Packet) {
				packet.UnsignedTx.TxIn[0].Sequence++
			},
		},
		{
			name: "outpoint",
			mutate: func(packet *psbt.Packet) {
				packet.UnsignedTx.TxIn[0].PreviousOutPoint.Hash =
					chainhash.Hash{1}
			},
		},
		{
			name: "output value",
			mutate: func(packet *psbt.Packet) {
				packet.UnsignedTx.TxOut[0].Value++
			},
		},
		{
			name: "output script",
			mutate: func(packet *psbt.Packet) {
				packet.UnsignedTx.TxOut[0].PkScript =
					[]byte{txscript.OP_FALSE}
			},
		},
		{
			name: "output order",
			mutate: func(packet *psbt.Packet) {
				packet.UnsignedTx.TxOut[0],
					packet.UnsignedTx.TxOut[1] =
					packet.UnsignedTx.TxOut[1],
					packet.UnsignedTx.TxOut[0]
				packet.Outputs[0], packet.Outputs[1] =
					packet.Outputs[1], packet.Outputs[0]
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			finalPacket, err := decodeAnchorPSBT(
				packageSnapshot.AnchorPsbt,
			)
			require.NoError(t, err)
			test.mutate(finalPacket)
			finalBytes, err := serializePSBT(finalPacket)
			require.NoError(t, err)

			_, err = wallet.PublishCustomAnchorTransfer(
				context.Background(), packageSnapshot, finalBytes,
			)
			require.ErrorContains(t, err, "verify final anchor PSBT")
		})
	}
	require.Zero(t, publishCalls)
}

func customAnchorSignVirtualPacket(t *testing.T,
	privateKey *btcec.PrivateKey, packetBytes []byte) []byte {

	t.Helper()
	packet, err := tappsbt.Decode(packetBytes)
	require.NoError(t, err)
	for _, input := range packet.Inputs {
		derivation, taprootDerivation :=
			tappsbt.Bip32DerivationFromKeyDesc(
				keychain.KeyDescriptor{PubKey: privateKey.PubKey()}, 1,
			)
		input.Bip32Derivation = []*psbt.Bip32Derivation{derivation}
		input.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{
			taprootDerivation,
		}
	}
	err = tapsend.SignVirtualTransaction(
		packet, tapscript.NewMockSigner(privateKey),
		customAnchorTestWitnessValidator{},
	)
	require.NoError(t, err)
	encoded, err := tappsbt.Encode(packet)
	require.NoError(t, err)
	return encoded
}

type customAnchorTestWitnessValidator struct{}

func (customAnchorTestWitnessValidator) ValidateWitnesses(
	newAsset *asset.Asset, splitAssets []*commitment.SplitAsset,
	prevAssets commitment.InputSet) error {

	return vm.ValidateWitnesses(newAsset, splitAssets, prevAssets)
}

func customAnchorTestCommitResponse(t *testing.T,
	req *CommitVirtualPsbtsRequest) *CommitVirtualPsbtsResponse {

	return customAnchorTestCommitResponseWithMutation(
		t, req, nil,
	)
}

func customAnchorTestCommitResponseWithMutation(t *testing.T,
	req *CommitVirtualPsbtsRequest,
	mutate func(*psbt.Packet, []*tappsbt.VPacket)) *CommitVirtualPsbtsResponse {

	t.Helper()
	anchor, err := decodeAnchorPSBT(req.AnchorPsbt)
	require.NoError(t, err)
	active, err := decodeVirtualPackets("active", req.VirtualPsbts)
	require.NoError(t, err)
	passive, err := decodeVirtualPackets(
		"passive", req.PassiveAssetPsbts,
	)
	require.NoError(t, err)
	allPackets := append(
		append([]*tappsbt.VPacket(nil), active...), passive...,
	)
	if mutate != nil {
		mutate(anchor, active)
	}
	outputCommitments, err := tapsend.CreateOutputCommitments(
		allPackets, tapsend.WithSpenderLeaves(),
	)
	require.NoError(t, err)
	for _, packet := range allPackets {
		require.NoError(t, tapsend.UpdateTaprootOutputKeys(
			anchor, packet, outputCommitments,
		))
	}
	for _, packet := range allPackets {
		for outputIndex := range packet.Outputs {
			suffix, err := tapsend.CreateProofSuffix(
				anchor.UnsignedTx, anchor.Outputs, packet,
				outputCommitments, outputIndex, allPackets,
				proof.WithVersion(proof.TransitionV1),
			)
			require.NoError(t, err)
			packet.Outputs[outputIndex].ProofSuffix = suffix
		}
	}

	anchorBytes, err := serializePSBT(anchor)
	require.NoError(t, err)
	activeBytes, err := encodeVirtualPackets(active)
	require.NoError(t, err)
	passiveBytes, err := encodeVirtualPackets(passive)
	require.NoError(t, err)

	return &CommitVirtualPsbtsResponse{
		AnchorPsbt:        anchorBytes,
		VirtualPsbts:      activeBytes,
		PassiveAssetPsbts: passiveBytes,
		ChangeOutputIndex: -1,
	}
}

func customAnchorMutateResponsePSBT(t *testing.T,
	response *CommitVirtualPsbtsResponse, mutate func(*psbt.Packet)) {

	t.Helper()
	packet, err := decodeAnchorPSBT(response.AnchorPsbt)
	require.NoError(t, err)
	mutate(packet)
	response.AnchorPsbt, err = serializePSBT(packet)
	require.NoError(t, err)
}

func customAnchorFinalizePSBT(t *testing.T, encoded []byte,
	keys ...*btcec.PrivateKey) []byte {

	t.Helper()

	packet, err := decodeAnchorPSBT(encoded)
	require.NoError(t, err)
	require.Len(t, keys, len(packet.Inputs))
	prevOutFetcher, prevOuts, err := anchorPrevOuts(packet)
	require.NoError(t, err)
	sigHashes := txscript.NewTxSigHashes(
		packet.UnsignedTx, prevOutFetcher,
	)
	for idx := range packet.Inputs {
		signature, err := txscript.RawTxInTaprootSignature(
			packet.UnsignedTx, sigHashes, idx, prevOuts[idx].Value,
			prevOuts[idx].PkScript, packet.Inputs[idx].TaprootMerkleRoot,
			packet.Inputs[idx].SighashType, keys[idx],
		)
		require.NoError(t, err)

		var witness bytes.Buffer
		err = psbt.WriteTxWitness(
			&witness, wire.TxWitness{signature},
		)
		require.NoError(t, err)
		packet.Inputs[idx].FinalScriptWitness = witness.Bytes()
	}
	finalized, err := serializePSBT(packet)
	require.NoError(t, err)
	extracted, err := psbt.Extract(packet)
	require.NoError(t, err)
	require.Len(t, extracted.TxIn, len(packet.Inputs))

	return finalized
}

func cloneCustomAnchorCommitRequest(
	req *CommitVirtualPsbtsRequest) *CommitVirtualPsbtsRequest {

	if req == nil {
		return nil
	}
	clone := *req
	clone.AnchorPsbt = cloneBytes(req.AnchorPsbt)
	clone.VirtualPsbts = cloneByteSlices(req.VirtualPsbts)
	clone.PassiveAssetPsbts = cloneByteSlices(req.PassiveAssetPsbts)
	clone.Funding.CustomLockID = cloneBytes(req.Funding.CustomLockID)
	return &clone
}

func cloneCustomAnchorPublishRequest(
	req *PublishAndLogTransferRequest) *PublishAndLogTransferRequest {

	if req == nil {
		return nil
	}
	clone := *req
	clone.AnchorPsbt = cloneBytes(req.AnchorPsbt)
	clone.VirtualPsbts = cloneByteSlices(req.VirtualPsbts)
	clone.PassiveAssetPsbts = cloneByteSlices(req.PassiveAssetPsbts)
	clone.LockedUTXOs = append([]Outpoint(nil), req.LockedUTXOs...)
	return &clone
}
