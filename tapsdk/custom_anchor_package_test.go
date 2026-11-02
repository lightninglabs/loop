package tapsdk

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	btcpsbt "github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/stretchr/testify/require"
)

func TestCustomAnchorTransferPackageSealValidate(t *testing.T) {
	fixture := newCustomAnchorTestFixture(t)
	pkg := fixture.unsealed

	require.Zero(t, pkg.SchemaVersion)
	require.Empty(t, pkg.CommittedPackageDigest)
	require.Empty(t, pkg.UnsignedTxDigest)
	require.Empty(t, pkg.PackageDigest)

	sealed, err := pkg.Seal()
	require.NoError(t, err)
	require.NoError(t, sealed.Validate())
	require.Equal(
		t, CustomAnchorTransferPackageVersion, sealed.SchemaVersion,
	)
	require.NotEmpty(t, sealed.CommittedPackageDigest)
	require.NotEmpty(t, sealed.UnsignedTxDigest)
	require.NotEmpty(t, sealed.PackageDigest)
	updateProof, err := proof.Decode(sealed.ProofUpdates[0].ProofBlob)
	require.NoError(t, err)
	require.Equal(t, proof.TransitionV1, updateProof.Version)
	active, err := tappsbt.Decode(sealed.ActiveVirtualPsbts[0])
	require.NoError(t, err)
	require.Equal(
		t, proof.TransitionV1,
		active.Outputs[0].ProofSuffix.Version,
	)

	// Sealing is immutable.
	require.Zero(t, pkg.SchemaVersion)
	require.Empty(t, pkg.CommittedPackageDigest)
	require.NotSame(t, pkg, sealed)
}

func TestCustomAnchorTransferPackageRejectsLegacyV0Proofs(t *testing.T) {
	t.Parallel()

	pkg := newCustomAnchorTestFixture(t).unsealed.Clone()
	setCustomAnchorPackageProofVersion(t, pkg, proof.TransitionV0)

	_, err := pkg.Seal()
	require.ErrorIs(t, err, proof.ErrTransitionV1Required)
}

func TestCustomAnchorTransferPackageSealErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CustomAnchorTransferPackage)
		wantErr string
	}{
		{
			name: "missing plan id",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.PlanID = Hash{}
			},
			wantErr: "plan ID is required",
		},
		{
			name: "missing anchor psbt",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.AnchorPsbt = nil
			},
			wantErr: "anchor PSBT",
		},
		{
			name: "malformed anchor psbt",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.AnchorPsbt = []byte{1, 2}
			},
			wantErr: "anchor PSBT",
		},
		{
			name: "missing active psbt",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.ActiveVirtualPsbts = nil
			},
			wantErr: "at least one active virtual PSBT is required",
		},
		{
			name: "empty active psbt",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.ActiveVirtualPsbts[0] = nil
			},
			wantErr: "active virtual PSBT 0 is empty",
		},
		{
			name: "empty passive psbt",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.PassiveVirtualPsbts = [][]byte{nil}
			},
			wantErr: "passive virtual PSBT 0 is empty",
		},
		{
			name: "change index below sentinel",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.ChangeOutputIndex = -2
			},
			wantErr: "change output index must be -1 or greater",
		},
		{
			name: "change index out of range",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.ChangeOutputIndex = 3
			},
			wantErr: "change output index is out of range",
		},
		{
			name: "lock expiration too long",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.FundingLock.LockExpirationSeconds =
					maxCustomAnchorLockExpirationSeconds + 1
			},
			wantErr: "lock expiration exceeds the maximum safe duration",
		},
		{
			name: "invalid input ref",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Inputs[0].AssetRef = AssetRef("invalid")
			},
			wantErr: "input 0 asset ref",
		},
		{
			name: "zero input amount",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Inputs[0].Amount = 0
			},
			wantErr: "input 0 amount is required",
		},
		{
			name: "input packet index out of range",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Inputs[0].PacketIndex = 2
			},
			wantErr: "input 0 packet index is out of range",
		},
		{
			name: "input anchor index out of range",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Inputs[0].AnchorInputIndex = 3
			},
			wantErr: "input 0 anchor input index is out of range",
		},
		{
			name: "invalid output ref",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Outputs[0].AssetRef = AssetRef("invalid")
			},
			wantErr: "output 0 asset ref",
		},
		{
			name: "zero output amount",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Outputs[0].Amount = 0
			},
			wantErr: "output 0 amount is required",
		},
		{
			name: "negative output anchor value",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Outputs[0].AnchorValueSat = -1
			},
			wantErr: "output 0 anchor value is negative",
		},
		{
			name: "output anchor index out of range",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Outputs[0].AnchorOutputIndex = 3
			},
			wantErr: "output 0 anchor output index is out of range",
		},
		{
			name: "OP_TRUE spend leaf changed",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Outputs[0].OPTrueSpend.LeafScript[0] =
					txscript.OP_FALSE
			},
			wantErr: "OP_TRUE spend leaf must be OP_TRUE",
		},
		{
			name: "OP_TRUE spend leaf version changed",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Outputs[0].OPTrueSpend.LeafVersion--
			},
			wantErr: "unsupported OP_TRUE spend leaf version",
		},
		{
			name: "OP_TRUE spend internal key changed",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Outputs[0].OPTrueSpend.InternalKey =
					customAnchorTestPubKey(
						customAnchorTestPrivateKey(20).PubKey(),
					)
			},
			wantErr: "internal key does not match control block",
		},
		{
			name: "OP_TRUE spend control block changed",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				controlBlock := pkg.Outputs[0].OPTrueSpend.ControlBlock
				controlBlock[len(controlBlock)-1] ^= 1
			},
			wantErr: "verify OP_TRUE spend commitment",
		},
		{
			name: "invalid proof update ref",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.ProofUpdates[0].AssetRef = AssetRef("invalid")
			},
			wantErr: "proof update 0 asset ref",
		},
		{
			name: "proof anchor index out of range",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.ProofUpdates[0].AnchorOutputIndex = 3
			},
			wantErr: "proof update 0 anchor output index is out of range",
		},
		{
			name: "missing signing plan",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans = pkg.SigningPlans[:2]
			},
			wantErr: "signing plans and backend-managed inputs must cover all 3",
		},
		{
			name: "backend-managed input out of range",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans = pkg.SigningPlans[:2]
				pkg.BackendManagedInputIndices = []uint32{3}
				pkg.FundingLock.CustomLockID = bytes.Repeat(
					[]byte{1}, 32,
				)
			},
			wantErr: "backend-managed input 0 index is out of range",
		},
		{
			name: "external and backend overlap",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans = pkg.SigningPlans[:2]
				pkg.BackendManagedInputIndices = []uint32{1}
				pkg.FundingLock.CustomLockID = bytes.Repeat(
					[]byte{1}, 32,
				)
			},
			wantErr: "anchor input 1 is classified more than once",
		},
		{
			name: "duplicate backend input",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans = pkg.SigningPlans[:1]
				pkg.BackendManagedInputIndices = []uint32{1, 1}
				pkg.FundingLock.CustomLockID = bytes.Repeat(
					[]byte{1}, 32,
				)
			},
			wantErr: "anchor input 1 is classified more than once",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkg := newCustomAnchorTestFixture(t).unsealed
			markCustomAnchorWalletFundedTestPackage(t, pkg)
			test.mutate(pkg)

			_, err := pkg.Seal()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorTransferPackageBackendManagedInput(t *testing.T) {
	pkg := newCustomAnchorTestFixture(t).unsealed
	markCustomAnchorWalletFundedTestPackage(t, pkg)
	pkg.SigningPlans = append(
		[]CustomAnchorInputSigningPlan(nil),
		pkg.SigningPlans[0], pkg.SigningPlans[2],
	)
	pkg.BackendManagedInputIndices = []uint32{1}
	anchor := mustDecodeAnchorPSBT(t, pkg.CommittedAnchorPsbt)
	pkg.LockedUTXOs = []CustomAnchorLockedUTXO{{
		Outpoint: outpointFromWire(
			anchor.UnsignedTx.TxIn[1].PreviousOutPoint,
		),
	}}

	sealed, err := pkg.Seal()
	require.NoError(t, err)
	require.NoError(t, sealed.Validate())
	require.Equal(t, []uint32{1}, sealed.BackendManagedInputIndices)

	requests, err := sealed.SigningRequests()
	require.NoError(t, err)
	require.Len(t, requests.KeyPath, 1)
	require.Empty(t, requests.MuSig2)
	require.Len(t, requests.ScriptPath, 1)
}

func TestCustomAnchorTransferPackageRejectsCurrentMetadataDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*btcpsbt.Packet)
	}{
		{
			name: "witness UTXO",
			mutate: func(packet *btcpsbt.Packet) {
				packet.Inputs[0].WitnessUtxo = cloneTxOut(
					packet.Inputs[0].WitnessUtxo,
				)
				packet.Inputs[0].WitnessUtxo.Value++
			},
		},
		{
			name: "sighash",
			mutate: func(packet *btcpsbt.Packet) {
				packet.Inputs[0].SighashType = txscript.SigHashSingle
			},
		},
		{
			name: "taproot metadata",
			mutate: func(packet *btcpsbt.Packet) {
				packet.Inputs[0].TaprootInternalKey = schnorr.SerializePubKey(
					testPrivateKey(t, 119).PubKey(),
				)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			pkg := newCustomAnchorTestFixture(t).unsealed
			current := mustDecodeAnchorPSBT(t, pkg.AnchorPsbt)
			test.mutate(current)
			pkg.AnchorPsbt = mustSerializeAnchorPSBT(t, current)

			_, err := pkg.Seal()
			require.ErrorContains(
				t, err, "metadata changed outside signature and "+
					"finalization fields",
			)
		})
	}
}

func TestCustomAnchorTransferPackageFundingLockInventory(t *testing.T) {
	baseline := newCustomAnchorWalletFundedTestPackage(t, 0)
	sealed, err := baseline.Seal()
	require.NoError(t, err)
	require.NoError(t, sealed.Validate())

	tests := []struct {
		name    string
		pkg     func(*testing.T) *CustomAnchorTransferPackage
		mutate  func(*CustomAnchorTransferPackage)
		wantErr string
	}{
		{
			name: "wallet funding without lock ID",
			pkg: func(t *testing.T) *CustomAnchorTransferPackage {
				pkg := newCustomAnchorWalletFundedTestPackage(t, 0)
				pkg.FundingLock = CustomAnchorFundingLockMetadata{}
				pkg.LockedUTXOs = nil
				pkg.BackendManagedInputIndices = nil

				return pkg
			},
			mutate:  func(*CustomAnchorTransferPackage) {},
			wantErr: "wallet-funded package requires a custom lock ID",
		},
		{
			name: "caller funding with lock metadata",
			pkg: func(t *testing.T) *CustomAnchorTransferPackage {
				pkg := newCustomAnchorTestFixture(t).unsealed
				pkg.Funding = CustomAnchorFundingSummary{
					Mode:         CustomAnchorFundingCallerFundedExact,
					ActualFeeSat: pkg.Funding.ActualFeeSat,
				}

				return pkg
			},
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.FundingLock.CustomLockID = bytes.Repeat([]byte{1}, 32)
			},
			wantErr: "caller-funded exact package cannot contain backend " +
				"funding metadata",
		},
		{
			name: "caller funding with change metadata",
			pkg: func(t *testing.T) *CustomAnchorTransferPackage {
				pkg := newCustomAnchorTestFixture(t).unsealed
				pkg.Funding = CustomAnchorFundingSummary{
					Mode:         CustomAnchorFundingCallerFundedExact,
					ActualFeeSat: pkg.Funding.ActualFeeSat,
				}

				return pkg
			},
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.ChangeOutputIndex = 0
			},
			wantErr: "caller-funded exact package cannot contain backend " +
				"funding metadata",
		},
		{
			name: "external P2A with change metadata",
			pkg: func(t *testing.T) *CustomAnchorTransferPackage {
				return newCustomAnchorTestFixture(t).unsealed
			},
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.ChangeOutputIndex = 0
			},
			wantErr: "external P2A package cannot contain backend funding " +
				"metadata",
		},
		{
			name: "expiration without lock ID",
			pkg: func(t *testing.T) *CustomAnchorTransferPackage {
				return newCustomAnchorTestFixture(t).unsealed
			},
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.FundingLock.LockExpirationSeconds = 1
			},
			wantErr: "backend-funded package requires a custom lock ID",
		},
		{
			name: "locked UTXO without lock ID",
			pkg: func(t *testing.T) *CustomAnchorTransferPackage {
				pkg := newCustomAnchorTestFixture(t).unsealed
				anchor := mustDecodeAnchorPSBT(t, pkg.CommittedAnchorPsbt)
				pkg.LockedUTXOs = []CustomAnchorLockedUTXO{{
					Outpoint: outpointFromWire(
						anchor.UnsignedTx.TxIn[0].PreviousOutPoint,
					),
				}}
				return pkg
			},
			mutate:  func(*CustomAnchorTransferPackage) {},
			wantErr: "backend-funded package requires a custom lock ID",
		},
		{
			name: "missing locked UTXO",
			pkg: func(t *testing.T) *CustomAnchorTransferPackage {
				return newCustomAnchorWalletFundedTestPackage(t, 0)
			},
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.LockedUTXOs = nil
			},
			wantErr: testDoNotCoverBackendFundedInputs,
		},
		{
			name: "duplicate locked UTXO",
			pkg: func(t *testing.T) *CustomAnchorTransferPackage {
				return newCustomAnchorWalletFundedTestPackage(t, 0)
			},
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.LockedUTXOs = append(
					pkg.LockedUTXOs, pkg.LockedUTXOs[0],
				)
			},
			wantErr: "locked UTXO 1 is duplicated",
		},
		{
			name: "extra locked UTXO",
			pkg: func(t *testing.T) *CustomAnchorTransferPackage {
				return newCustomAnchorWalletFundedTestPackage(t, 0)
			},
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.LockedUTXOs = append(
					pkg.LockedUTXOs, CustomAnchorLockedUTXO{
						Outpoint: Outpoint{Txid: [32]byte{99}},
					},
				)
			},
			wantErr: testDoNotCoverBackendFundedInputs,
		},
		{
			name: "inventory differs from backend inputs",
			pkg: func(t *testing.T) *CustomAnchorTransferPackage {
				pkg := newCustomAnchorWalletFundedTestPackage(t, 1)
				other := newCustomAnchorWalletFundedTestPackage(t, 0)
				pkg.LockedUTXOs = other.LockedUTXOs
				return pkg
			},
			mutate:  func(*CustomAnchorTransferPackage) {},
			wantErr: testDoNotCoverBackendFundedInputs,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkg := test.pkg(t)
			test.mutate(pkg)

			_, err := pkg.Seal()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorTransferPackageRejectsSemanticTamper(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*testing.T, *CustomAnchorTransferPackage)
		wantErr string
	}{
		{
			name: "input previous ID asset ID",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRoleActive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Inputs[0].PrevID.ID[0] ^= 1
					},
				)
			},
			wantErr: testValidateVirtualPacketWitnesses,
		},
		{
			name: "input previous ID script key",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRoleActive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Inputs[0].PrevID.ScriptKey =
							asset.ToSerialized(
								testPrivateKey(t, 91).PubKey(),
							)
					},
				)
			},
			wantErr: testValidateVirtualPacketWitnesses,
		},
		{
			name: "input previous ID outpoint",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRoleActive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Inputs[0].PrevID.OutPoint.Hash[0] ^= 1
					},
				)
			},
			wantErr: testValidateVirtualPacketWitnesses,
		},
		{
			name: "output asset amount",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRoleActive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Outputs[0].Asset.Amount--
					},
				)
			},
			wantErr: testValidateVirtualPacketWitnesses,
		},
		{
			name: "output asset script key",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRoleActive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Outputs[0].Asset.ScriptKey =
							asset.NewScriptKey(
								testPrivateKey(t, 92).PubKey(),
							)
					},
				)
			},
			wantErr: testValidateVirtualPacketWitnesses,
		},
		{
			name: "output proof courier",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRoleActive, 0,
					func(packet *tappsbt.VPacket) {
						var err error
						packet.Outputs[0].ProofDeliveryAddress, err =
							url.Parse("universerpc://other.example")
						require.NoError(t, err)
					},
				)
			},
			wantErr: testOutputProofCourierDoesNotMatchOutputSummary,
		},
		{
			name: "output taproot asset root",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				pkg.Outputs[0].TaprootAssetRoot[0] ^= 1
			},
			wantErr: "output commitment roots do not match virtual output",
		},
		{
			name: "output taproot merkle root",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				pkg.Outputs[0].TaprootMerkleRoot[0] ^= 1
			},
			wantErr: "output commitment roots do not match virtual output",
		},
		{
			name: "output anchor index",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRoleActive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Outputs[0].AnchorOutputIndex = 0
					},
				)
			},
			wantErr: "package anchor outputs",
		},
		{
			name: "virtual proof suffix",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRoleActive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Outputs[0].ProofSuffix.AnchorTx.
							LockTime++
					},
				)
			},
			wantErr: testProofSuffixDoesNotMatchCommittedAnchor,
		},
		{
			name: "proof update suffix",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				transition, err := proof.Decode(
					pkg.ProofUpdates[0].ProofBlob,
				)
				require.NoError(t, err)
				transition.AnchorTx.LockTime++
				pkg.ProofUpdates[0].ProofBlob, err =
					encodeTransitionProof(transition)
				require.NoError(t, err)
			},
			wantErr: testProofSuffixDoesNotMatchCommittedAnchor,
		},
		{
			name: "unsupported proof update version",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				transition, err := proof.Decode(
					pkg.ProofUpdates[0].ProofBlob,
				)
				require.NoError(t, err)
				transition.Version = proof.TransitionVersion(2)
				pkg.ProofUpdates[0].ProofBlob, err =
					encodeTransitionProof(transition)
				require.NoError(t, err)
			},
			wantErr: "unsupported proof update transition proof version 2",
		},
		{
			name: "unsupported virtual proof version",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRoleActive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Outputs[0].ProofSuffix.Version =
							proof.TransitionVersion(2)
					},
				)
			},
			wantErr: "unsupported virtual output transition proof version 2",
		},
		{
			name: "proof suffix version mismatch",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				transition, err := proof.Decode(
					pkg.ProofUpdates[0].ProofBlob,
				)
				require.NoError(t, err)
				transition.Version = proof.TransitionV0
				pkg.ProofUpdates[0].ProofBlob, err =
					encodeTransitionProof(transition)
				require.NoError(t, err)
			},
			wantErr: "proof suffix transition proof versions do not match",
		},
		{
			name: "missing input mapping",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				pkg.Inputs = nil
			},
			wantErr: "has no package mapping",
		},
		{
			name: "duplicate input mapping",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				pkg.Inputs = append(pkg.Inputs, pkg.Inputs[0])
			},
			wantErr: "duplicates a virtual input mapping",
		},
		{
			name: "extra input mapping",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				extra := pkg.Inputs[0]
				extra.LogicalInputID = "extra-input"
				extra.LogicalInputIndex++
				extra.VirtualInputIndex++
				pkg.Inputs = append(pkg.Inputs, extra)
			},
			wantErr: "do not exactly cover virtual inputs",
		},
		{
			name: "proof source tip",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				otherFile, _, _ := newGroupedAssetProofPathBase(t)
				pkg.Inputs[0].ProofSource.Blob = otherFile
				pkg.Inputs[0].ProofSource.ContentID = customAnchorDigest(
					customAnchorProofFileDigestDomain, otherFile,
				)
			},
			wantErr: "proof source tip does not match input summary",
		},
		{
			name: "missing proof source",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				pkg.Inputs[0].ProofSource.Blob = nil
			},
			wantErr: "proof source blob is required",
		},
		{
			name: "missing output mapping",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				pkg.Outputs = nil
			},
			wantErr: "has no package mapping",
		},
		{
			name: "duplicate output mapping",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				pkg.Outputs = append(pkg.Outputs, pkg.Outputs[0])
			},
			wantErr: "duplicates a virtual output mapping",
		},
		{
			name: "extra output mapping",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				extra := pkg.Outputs[0]
				extra.LogicalOutputID = "extra-output"
				extra.LogicalOutputIndex++
				extra.VirtualOutputIndex++
				pkg.Outputs = append(pkg.Outputs, extra)
			},
			wantErr: "do not exactly cover virtual outputs",
		},
		{
			name: "missing proof update",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				pkg.ProofUpdates = nil
			},
			wantErr: "has no proof update",
		},
		{
			name: "duplicate proof update",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				pkg.ProofUpdates = append(
					pkg.ProofUpdates, pkg.ProofUpdates[0],
				)
			},
			wantErr: "duplicates a virtual output mapping",
		},
		{
			name: "extra proof update",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				extra := pkg.ProofUpdates[0]
				extra.LogicalOutputID = "extra-output"
				extra.LogicalOutputIndex++
				extra.VirtualOutputIndex++
				pkg.ProofUpdates = append(pkg.ProofUpdates, extra)
			},
			wantErr: "do not exactly cover virtual outputs",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkg := newCustomAnchorTestFixture(t).unsealed
			test.mutate(t, pkg)

			_, err := pkg.Seal()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorTransferPackageRejectsRootHintTamper(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func([]*btcpsbt.Unknown) []*btcpsbt.Unknown
		wantErr string
	}{
		{
			name: "missing asset root",
			mutate: func(unknowns []*btcpsbt.Unknown) []*btcpsbt.Unknown {
				return removeCustomAnchorTestField(
					unknowns, tappsbt.PsbtKeyTypeOutputAssetRoot,
				)
			},
			wantErr: "Taproot Asset root hint must be 32 bytes, was 0",
		},
		{
			name: "short merkle root",
			mutate: func(unknowns []*btcpsbt.Unknown) []*btcpsbt.Unknown {
				return tappsbt.AddCustomField(
					unknowns,
					tappsbt.PsbtKeyTypeOutputTaprootMerkleRoot,
					[]byte{1},
				)
			},
			wantErr: "Taproot merkle root hint must be 32 bytes, was 1",
		},
		{
			name: "wrong asset root",
			mutate: func(unknowns []*btcpsbt.Unknown) []*btcpsbt.Unknown {
				return tappsbt.AddCustomField(
					unknowns, tappsbt.PsbtKeyTypeOutputAssetRoot,
					bytes.Repeat([]byte{1}, 32),
				)
			},
			wantErr: "Taproot Asset root hint does not match",
		},
		{
			name: "wrong merkle root",
			mutate: func(unknowns []*btcpsbt.Unknown) []*btcpsbt.Unknown {
				return tappsbt.AddCustomField(
					unknowns,
					tappsbt.PsbtKeyTypeOutputTaprootMerkleRoot,
					bytes.Repeat([]byte{2}, 32),
				)
			},
			wantErr: "Taproot merkle root hint does not match",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkg := newCustomAnchorTestFixture(t).unsealed
			for _, current := range []*[]byte{
				&pkg.CommittedAnchorPsbt, &pkg.AnchorPsbt,
			} {
				packet := mustDecodeAnchorPSBT(t, *current)
				index := pkg.Outputs[0].AnchorOutputIndex
				packet.Outputs[index].Unknowns = test.mutate(
					packet.Outputs[index].Unknowns,
				)
				*current = mustSerializeAnchorPSBT(t, packet)
			}

			_, err := pkg.Seal()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorTransferPackageRejectsPassiveSemanticTamper(
	t *testing.T) {

	tests := []struct {
		name    string
		mutate  func(*testing.T, *CustomAnchorTransferPackage)
		wantErr string
	}{
		{
			name: "previous ID asset ID",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRolePassive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Inputs[0].PrevID.ID[0] ^= 1
					},
				)
			},
			wantErr: testValidateVirtualPacketWitnesses,
		},
		{
			name: "output asset amount",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRolePassive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Outputs[0].Asset.Amount--
					},
				)
			},
			wantErr: testValidateVirtualPacketWitnesses,
		},
		{
			name: "output asset script key",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRolePassive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Outputs[0].Asset.ScriptKey =
							asset.NewScriptKey(
								testPrivateKey(t, 93).PubKey(),
							)
					},
				)
			},
			wantErr: testValidateVirtualPacketWitnesses,
		},
		{
			name: "output anchor index",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRolePassive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Outputs[0].AnchorOutputIndex = 0
					},
				)
			},
			wantErr: "package anchor outputs",
		},
		{
			name: "proof suffix",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				mutateCustomAnchorPackageVPacket(
					t, pkg, CustomAnchorPacketRolePassive, 0,
					func(packet *tappsbt.VPacket) {
						packet.Outputs[0].ProofSuffix.AnchorTx.
							LockTime++
					},
				)
			},
			wantErr: testProofSuffixDoesNotMatchCommittedAnchor,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkg := newCommittedCustomAnchorPassivePackage(t)
			test.mutate(t, pkg)

			_, err := pkg.Seal()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorTransferPackageProofSuffixAltLeafOrder(t *testing.T) {
	pkg := newCommittedCustomAnchorPassivePackage(t)
	packet, err := tappsbt.Decode(pkg.ActiveVirtualPsbts[0])
	require.NoError(t, err)
	require.NotNil(t, packet.Outputs[0].ProofSuffix)
	leaves := packet.Outputs[0].ProofSuffix.AltLeaves
	require.GreaterOrEqual(t, len(leaves), 2)

	firstKey := leaves[0].AssetCommitmentKey()
	lastKey := leaves[len(leaves)-1].AssetCommitmentKey()
	leaves[0], leaves[len(leaves)-1] = leaves[len(leaves)-1], leaves[0]
	pkg.ActiveVirtualPsbts[0], err = tappsbt.Encode(packet)
	require.NoError(t, err)

	reordered, err := tappsbt.Decode(pkg.ActiveVirtualPsbts[0])
	require.NoError(t, err)
	require.Equal(t, lastKey,
		reordered.Outputs[0].ProofSuffix.AltLeaves[0].AssetCommitmentKey())
	require.Equal(t, firstKey,
		reordered.Outputs[0].ProofSuffix.AltLeaves[len(leaves)-1].
			AssetCommitmentKey())

	// An MS-SMT commits alternate leaves by key, so only changing their
	// serialized order must not invalidate an otherwise identical suffix.
	resealed, err := pkg.Seal()
	require.NoError(t, err)
	require.NoError(t, resealed.Validate())

	mutated := newCommittedCustomAnchorPassivePackage(t)
	mutateCustomAnchorPackageVPacket(
		t, mutated, CustomAnchorPacketRoleActive, 0,
		func(packet *tappsbt.VPacket) {
			altLeaves := asset.FromAltLeaves(
				packet.Outputs[0].ProofSuffix.AltLeaves,
			)
			require.GreaterOrEqual(t, len(altLeaves), 2)
			altLeaves[0].ScriptKey = asset.NewScriptKey(
				testPrivateKey(t, 94).PubKey(),
			)
			packet.Outputs[0].ProofSuffix.AltLeaves[0],
				packet.Outputs[0].ProofSuffix.AltLeaves[1] =
				packet.Outputs[0].ProofSuffix.AltLeaves[1],
				packet.Outputs[0].ProofSuffix.AltLeaves[0]
		},
	)

	_, err = mutated.Seal()
	require.ErrorContains(
		t, err, testProofSuffixDoesNotMatchCommittedAnchor,
	)
}

func TestCustomAnchorTransferPackageRejectsSplitAssetTamper(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *tappsbt.VOutput)
	}{
		{
			name: "amount",
			mutate: func(_ *testing.T, output *tappsbt.VOutput) {
				output.SplitAsset.Amount--
			},
		},
		{
			name: "script key",
			mutate: func(t *testing.T, output *tappsbt.VOutput) {
				output.SplitAsset.ScriptKey = asset.NewScriptKey(
					testPrivateKey(t, 95).PubKey(),
				)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkg := newCustomAnchorSplitTestPackage(t)
			mutateCustomAnchorPackageVPacket(
				t, pkg, CustomAnchorPacketRoleActive, 0,
				func(packet *tappsbt.VPacket) {
					var splitOutput *tappsbt.VOutput
					for _, output := range packet.Outputs {
						if output.SplitAsset != nil {
							splitOutput = output
							break
						}
					}
					require.NotNil(t, splitOutput)
					test.mutate(t, splitOutput)
				},
			)

			_, err := pkg.Seal()
			require.ErrorContains(
				t, err, testValidateVirtualPacketWitnesses,
			)
		})
	}
}

func TestCustomAnchorTransferPackageValidateTamper(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CustomAnchorTransferPackage)
		wantErr string
	}{
		{
			name: "unsupported version",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SchemaVersion++
			},
			wantErr: "unsupported custom anchor package version",
		},
		{
			name: "unsigned transaction digest",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.UnsignedTxDigest[0] ^= 1
			},
			wantErr: "unsigned transaction digest mismatch",
		},
		{
			name: "committed metadata",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Publish.Label = "tampered"
			},
			wantErr: testCommittedPackageDigestMismatch,
		},
		{
			name: "actual funding fee",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.Funding.ActualFeeSat++
			},
			wantErr: "funding summary fee",
		},
		{
			name: "P2A funding index",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				index := uint32(0)
				pkg.Funding.P2AOutputIndex = &index
			},
			wantErr: "not the canonical zero-value P2A",
		},
		{
			name: "committed digest",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.CommittedPackageDigest[0] ^= 1
			},
			wantErr: testCommittedPackageDigestMismatch,
		},
		{
			name: "current digest",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.PackageDigest[0] ^= 1
			},
			wantErr: "package digest mismatch",
		},
		{
			name: "signature field",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				packet := mustDecodeAnchorPSBT(t, pkg.AnchorPsbt)
				packet.Inputs[0].TaprootKeySpendSig = bytes.Repeat(
					[]byte{1}, schnorr.SignatureSize,
				)
				pkg.AnchorPsbt = mustSerializeAnchorPSBT(t, packet)
			},
			wantErr: "package digest mismatch",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkg := newCustomAnchorTestFixture(t).sealed.Clone()
			test.mutate(pkg)

			err := pkg.Validate()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorTransferPackageRoundTrip(t *testing.T) {
	pkg := newCustomAnchorTestFixture(t).sealed

	jsonOne, err := json.Marshal(pkg)
	require.NoError(t, err)
	jsonTwo, err := json.Marshal(pkg)
	require.NoError(t, err)
	require.JSONEq(t, string(jsonOne), string(jsonTwo))

	var jsonDecoded CustomAnchorTransferPackage
	require.NoError(t, json.Unmarshal(jsonOne, &jsonDecoded))
	require.Equal(t, pkg, &jsonDecoded)
	jsonRoundTrip, err := json.Marshal(&jsonDecoded)
	require.NoError(t, err)
	require.JSONEq(t, string(jsonOne), string(jsonRoundTrip))

	binaryOne, err := pkg.MarshalBinary()
	require.NoError(t, err)
	binaryTwo, err := pkg.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, binaryOne, binaryTwo)

	var binaryDecoded CustomAnchorTransferPackage
	require.NoError(t, binaryDecoded.UnmarshalBinary(binaryOne))
	require.Equal(t, pkg, &binaryDecoded)
	binaryRoundTrip, err := binaryDecoded.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, binaryOne, binaryRoundTrip)
}

func TestCustomAnchorTransferPackageDecodeErrors(t *testing.T) {
	pkg := newCustomAnchorTestFixture(t).sealed
	jsonBytes, err := pkg.MarshalJSON()
	require.NoError(t, err)
	binaryBytes, err := pkg.MarshalBinary()
	require.NoError(t, err)

	t.Run("json tamper", func(t *testing.T) {
		tampered := bytes.Replace(
			jsonBytes, []byte("custom-anchor"), []byte("custom-Xnchor"),
			1,
		)
		var decoded CustomAnchorTransferPackage
		err := json.Unmarshal(tampered, &decoded)
		require.ErrorContains(t, err, testCommittedPackageDigestMismatch)
	})

	t.Run("json unknown field", func(t *testing.T) {
		tampered := append([]byte(nil), jsonBytes[:len(jsonBytes)-1]...)
		tampered = append(tampered, []byte(`,"unknown":true}`)...)
		var decoded CustomAnchorTransferPackage
		err := json.Unmarshal(tampered, &decoded)
		require.ErrorContains(t, err, "unknown field")
	})

	t.Run("json trailing value", func(t *testing.T) {
		var decoded CustomAnchorTransferPackage
		err := json.Unmarshal(
			append(jsonBytes, []byte(` {}`)...), &decoded,
		)
		require.Error(t, err)
	})

	tests := []struct {
		name    string
		mutate  func([]byte) []byte
		wantErr string
	}{
		{
			name: "short",
			mutate: func(_ []byte) []byte {
				return []byte{1}
			},
			wantErr: "binary envelope is short",
		},
		{
			name: "magic",
			mutate: func(data []byte) []byte {
				data[0] ^= 1
				return data
			},
			wantErr: "binary magic",
		},
		{
			name: testVersion,
			mutate: func(data []byte) []byte {
				binary.BigEndian.PutUint16(
					data[len(customAnchorPackageMagic):], 2,
				)
				return data
			},
			wantErr: "unsupported custom anchor package version 2",
		},
		{
			name: "length",
			mutate: func(data []byte) []byte {
				data = append(data, 0)
				return data
			},
			wantErr: "binary length mismatch",
		},
		{
			name: "payload tamper",
			mutate: func(data []byte) []byte {
				index := bytes.Index(data, []byte("custom-anchor"))
				require.NotEqual(t, -1, index)
				data[index] ^= 1
				return data
			},
			wantErr: testCommittedPackageDigestMismatch,
		},
	}

	for _, test := range tests {
		t.Run("binary "+test.name, func(t *testing.T) {
			data := test.mutate(append([]byte(nil), binaryBytes...))
			var decoded CustomAnchorTransferPackage
			err := decoded.UnmarshalBinary(data)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorTransferPackageDecodeIsAtomic(t *testing.T) {
	t.Parallel()

	pkg := newCustomAnchorTestFixture(t).sealed
	jsonBytes, err := pkg.MarshalJSON()
	require.NoError(t, err)
	binaryBytes, err := pkg.MarshalBinary()
	require.NoError(t, err)

	t.Run("json", func(t *testing.T) {
		receiver := *pkg.Clone()
		before := receiver.Clone()
		tampered := bytes.Replace(
			jsonBytes, []byte("custom-anchor"), []byte("custom-Xnchor"),
			1,
		)
		err := json.Unmarshal(tampered, &receiver)
		require.Error(t, err)
		require.Equal(t, before, &receiver)
	})

	t.Run("binary", func(t *testing.T) {
		receiver := *pkg.Clone()
		before := receiver.Clone()
		tampered := bytes.Replace(
			binaryBytes, []byte("custom-anchor"), []byte("custom-Xnchor"),
			1,
		)
		err := receiver.UnmarshalBinary(tampered)
		require.Error(t, err)
		require.Equal(t, before, &receiver)
	})
}

func TestCustomAnchorTransferPackageClone(t *testing.T) {
	pkg := newCustomAnchorTestFixture(t).sealed.Clone()
	pkg.BackendManagedInputIndices = []uint32{1}
	pkg.PassiveVirtualPsbts = [][]byte{{1}}
	clone := pkg.Clone()

	pkg.CommittedAnchorPsbt[0] ^= 1
	pkg.AnchorPsbt[0] ^= 1
	pkg.ActiveVirtualPsbts[0][0] ^= 1
	pkg.PassiveVirtualPsbts[0][0] ^= 1
	pkg.Outputs[0].OPTrueSpend.LeafScript[0] = txscript.OP_FALSE
	pkg.Outputs[0].OPTrueSpend.ControlBlock[0] ^= 1
	pkg.ProofUpdates[0].ProofBlob[0] ^= 1
	pkg.SigningPlans[1].MuSig2.Participants[0][0] ^= 1
	pkg.SigningPlans[1].MuSig2.SessionContext[0] ^= 1
	pkg.SigningPlans[2].ScriptPath.RequiredSigners = append(
		pkg.SigningPlans[2].ScriptPath.RequiredSigners,
		XOnlyPubKey{1},
	)
	pkg.BackendManagedInputIndices[0] = 2

	require.NotEqual(t, pkg.CommittedAnchorPsbt, clone.CommittedAnchorPsbt)
	require.NotEqual(t, pkg.AnchorPsbt, clone.AnchorPsbt)
	require.NotEqual(
		t, pkg.ActiveVirtualPsbts[0], clone.ActiveVirtualPsbts[0],
	)
	require.NotEqual(
		t, pkg.PassiveVirtualPsbts[0], clone.PassiveVirtualPsbts[0],
	)
	require.Equal(t, []byte{txscript.OP_TRUE},
		clone.Outputs[0].OPTrueSpend.LeafScript)
	require.NotEqual(t, pkg.Outputs[0].OPTrueSpend.ControlBlock,
		clone.Outputs[0].OPTrueSpend.ControlBlock)
	require.NotEqual(
		t, pkg.ProofUpdates[0].ProofBlob,
		clone.ProofUpdates[0].ProofBlob,
	)
	require.NotEqual(
		t, pkg.SigningPlans[1].MuSig2.Participants,
		clone.SigningPlans[1].MuSig2.Participants,
	)
	require.NotEqual(
		t, pkg.SigningPlans[1].MuSig2.SessionContext,
		clone.SigningPlans[1].MuSig2.SessionContext,
	)
	require.Empty(t, clone.SigningPlans[2].ScriptPath.RequiredSigners)
	require.Equal(t, []uint32{1}, clone.BackendManagedInputIndices)
}

func TestCustomAnchorTransferPackageNil(t *testing.T) {
	var pkg *CustomAnchorTransferPackage

	sealed, err := pkg.Seal()
	require.Nil(t, sealed)
	require.ErrorContains(t, err, "nil custom anchor transfer package")
	require.ErrorContains(
		t, pkg.Validate(), "nil custom anchor transfer package",
	)
	require.Nil(t, pkg.Clone())
}

type customAnchorTestFixture struct {
	unsealed          *CustomAnchorTransferPackage
	sealed            *CustomAnchorTransferPackage
	keyPathPrivateKey *btcec.PrivateKey
	muSigPrivateKeys  []*btcec.PrivateKey
}

func newCustomAnchorTestFixture(t *testing.T) *customAnchorTestFixture {
	t.Helper()

	fixture := newCustomAnchorBuilderFixture(t)
	request := fixture.request.Clone()
	anchor := mustDecodeAnchorPSBT(t, request.AnchorPSBT)

	// Put the asset input first so the key-path signing tests exercise a
	// Taproot Asset anchor input, then repurpose the caller's BTC input for
	// MuSig2.
	anchor.UnsignedTx.TxIn[0], anchor.UnsignedTx.TxIn[1] =
		anchor.UnsignedTx.TxIn[1], anchor.UnsignedTx.TxIn[0]
	anchor.Inputs[0], anchor.Inputs[1] = anchor.Inputs[1], anchor.Inputs[0]

	keyPathPrivateKey := testPrivateKey(t, 2)
	require.Equal(
		t, schnorr.SerializePubKey(keyPathPrivateKey.PubKey()),
		schnorr.SerializePubKey(
			fixture.inputProof.InclusionProof.InternalKey,
		),
	)
	muSigPrivateKeys := []*btcec.PrivateKey{
		customAnchorTestPrivateKey(2),
		customAnchorTestPrivateKey(3),
	}
	muSigKeys := []*btcec.PublicKey{
		muSigPrivateKeys[0].PubKey(), muSigPrivateKeys[1].PubKey(),
	}
	muSigAggregate, _, _, err := musig2.AggregateKeys(muSigKeys, false)
	require.NoError(t, err)
	muSigOutput := txscript.ComputeTaprootKeyNoScript(
		muSigAggregate.PreTweakedKey,
	)
	muSigScript, err := txscript.PayToTaprootScript(muSigOutput)
	require.NoError(t, err)
	anchor.Inputs[1] = btcpsbt.PInput{
		WitnessUtxo: &wire.TxOut{
			Value:    anchor.Inputs[1].WitnessUtxo.Value,
			PkScript: muSigScript,
		},
		TaprootInternalKey: schnorr.SerializePubKey(
			muSigAggregate.PreTweakedKey,
		),
	}

	scriptInternalKey := customAnchorTestPrivateKey(4)
	tapLeaf := txscript.NewBaseTapLeaf([]byte{txscript.OP_TRUE})
	scriptTree := txscript.AssembleTaprootScriptTree(tapLeaf)
	scriptRoot := scriptTree.RootNode.TapHash()
	scriptOutput := txscript.ComputeTaprootOutputKey(
		scriptInternalKey.PubKey(), scriptRoot[:],
	)
	scriptPathScript, err := txscript.PayToTaprootScript(scriptOutput)
	require.NoError(t, err)
	controlBlock := scriptTree.LeafMerkleProofs[0].ToControlBlock(
		scriptInternalKey.PubKey(),
	)
	controlBlockBytes, err := controlBlock.ToBytes()
	require.NoError(t, err)

	anchor.UnsignedTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{3},
			Index: 2,
		},
		Sequence: wire.MaxTxInSequenceNum - 2,
	})
	anchor.Inputs = append(anchor.Inputs, btcpsbt.PInput{
		WitnessUtxo: &wire.TxOut{
			Value:    1_000,
			PkScript: scriptPathScript,
		},
		TaprootInternalKey: schnorr.SerializePubKey(
			scriptInternalKey.PubKey(),
		),
		TaprootMerkleRoot: append([]byte(nil), scriptRoot[:]...),
		TaprootLeafScript: []*btcpsbt.TaprootTapLeafScript{{
			ControlBlock: controlBlockBytes,
			Script:       append([]byte(nil), tapLeaf.Script...),
			LeafVersion:  tapLeaf.LeafVersion,
		}},
	})
	// Keep the external P2A parent exactly zero fee after adding the script
	// path test input.
	anchor.UnsignedTx.TxOut[0].Value += 1_000

	destinationKey := customAnchorTestPrivateKey(5)
	request.AnchorPSBT = mustSerializeAnchorPSBT(t, anchor)
	request.Outputs[0].Script = CustomAssetScriptPlan{
		Mode: CustomAssetScriptOPTrue,
		OPTrue: &CustomAssetOPTrueScriptPlan{
			InternalKey: KeyDescriptor{
				RawKeyBytes: mustCustomAnchorPubKey(
					t, destinationKey.PubKey(),
				),
			},
		},
	}
	request.Outputs[0].ProofDelivery = CustomAssetProofDelivery{
		CourierAddress: "universerpc://proof.example",
	}

	leafHash := tapLeaf.TapHash()
	request.SigningPlans = []CustomAnchorInputSigningPlan{
		{
			InputIndex: 0,
			KeyPath: &CustomAnchorKeyPathSigningPlan{
				Signer: customAnchorTestXOnly(keyPathPrivateKey.PubKey()),
			},
		},
		{
			InputIndex: 1,
			MuSig2: &CustomAnchorMuSig2SigningPlan{
				Participants: []PubKey{
					customAnchorTestPubKey(muSigKeys[0]),
					customAnchorTestPubKey(muSigKeys[1]),
				},
				SessionContext: []byte("batch-session-1"),
			},
		},
		{
			InputIndex: 2,
			ScriptPath: &CustomAnchorScriptPathSigningPlan{
				LeafHash: Hash(leafHash),
			},
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

		return customAnchorTestCommitResponse(t, req), nil
	}
	capabilities := DefaultTapdCustomAnchorCapabilities()
	client := &capableCustomAnchorBuilderTestClient{
		customAnchorBuilderTestClient: fixture.client,
		capabilities:                  &capabilities,
	}
	plan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(context.Background(), request)
	require.NoError(t, err)

	sealed, err := plan.Commit(
		context.Background(), CustomAnchorCommitOptions{
			Publish: CustomAnchorPublishMetadata{
				SkipAnchorTxBroadcast: true,
				Label:                 "custom-anchor",
				ExternalBroadcast:     true,
			},
		},
	)
	require.NoError(t, err)

	unsealed := sealed.Clone()
	unsealed.SchemaVersion = 0
	unsealed.CommittedPackageDigest = Hash{}
	unsealed.UnsignedTxDigest = Hash{}
	unsealed.PackageDigest = Hash{}

	return &customAnchorTestFixture{
		unsealed:          unsealed,
		sealed:            sealed,
		keyPathPrivateKey: keyPathPrivateKey,
		muSigPrivateKeys:  muSigPrivateKeys,
	}
}

func setCustomAnchorPackageProofVersion(t *testing.T,
	pkg *CustomAnchorTransferPackage, version proof.TransitionVersion) {

	t.Helper()

	anchor := mustDecodeAnchorPSBT(t, pkg.AnchorPsbt)
	active, err := decodeVirtualPackets("active", pkg.ActiveVirtualPsbts)
	require.NoError(t, err)
	passive, err := decodeVirtualPackets(
		"passive", pkg.PassiveVirtualPsbts,
	)
	require.NoError(t, err)
	allPackets := append(
		append([]*tappsbt.VPacket(nil), active...), passive...,
	)
	commitments, err := tapsend.CreateOutputCommitments(
		allPackets, tapsend.WithNoSTXOProofs(),
	)
	require.NoError(t, err)

	for _, packet := range allPackets {
		for outputIndex := range packet.Outputs {
			suffix, err := tapsend.CreateProofSuffix(
				anchor.UnsignedTx, anchor.Outputs, packet,
				commitments, outputIndex, allPackets,
				proof.WithVersion(version),
			)
			require.NoError(t, err)
			packet.Outputs[outputIndex].ProofSuffix = suffix
		}
	}

	pkg.ActiveVirtualPsbts, err = encodeVirtualPackets(active)
	require.NoError(t, err)
	pkg.PassiveVirtualPsbts, err = encodeVirtualPackets(passive)
	require.NoError(t, err)
	for idx := range pkg.ProofUpdates {
		update := &pkg.ProofUpdates[idx]
		packets := active
		if update.PacketRole == CustomAnchorPacketRolePassive {
			packets = passive
		}
		require.Less(t, update.PacketIndex, uint32(len(packets)))
		packet := packets[update.PacketIndex]
		require.Less(
			t, update.VirtualOutputIndex,
			uint32(len(packet.Outputs)),
		)
		update.ProofBlob, err = encodeTransitionProof(
			packet.Outputs[update.VirtualOutputIndex].ProofSuffix,
		)
		require.NoError(t, err)
	}
}

func customAnchorTestPrivateKey(value byte) *btcec.PrivateKey {
	keyBytes := make([]byte, 32)
	keyBytes[len(keyBytes)-1] = value
	privateKey, _ := btcec.PrivKeyFromBytes(keyBytes)
	return privateKey
}

func customAnchorTestXOnly(key *btcec.PublicKey) XOnlyPubKey {
	var result XOnlyPubKey
	copy(result[:], schnorr.SerializePubKey(key))
	return result
}

func customAnchorTestPubKey(key *btcec.PublicKey) PubKey {
	var result PubKey
	copy(result[:], key.SerializeCompressed())
	return result
}

func newCustomAnchorSplitTestPackage(
	t *testing.T) *CustomAnchorTransferPackage {

	t.Helper()

	fixture := newCustomAnchorBuilderFixture(t)
	request := fixture.request.Clone()
	first := request.Outputs[0]
	first.Amount = 60
	second := first
	second.ID = "asset-change"
	second.Amount = 40
	second.AnchorOutputIndex = 0
	second.AnchorValueSat = 12_345
	second.Script = CustomAssetScriptPlan{
		Mode: CustomAssetScriptExternal,
		External: &CustomAssetExternalScriptPlan{
			ScriptKey: customAnchorExternalScriptKey(
				t, testPrivateKey(t, 96),
			),
		},
	}
	request.Outputs = []CustomAssetOutput{first, second}

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
	capabilities := DefaultTapdCustomAnchorCapabilities()
	client := &capableCustomAnchorBuilderTestClient{
		customAnchorBuilderTestClient: fixture.client,
		capabilities:                  &capabilities,
	}
	plan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(context.Background(), request)
	require.NoError(t, err)
	pkg, err := plan.Commit(context.Background())
	require.NoError(t, err)
	require.NoError(t, pkg.Validate())

	return pkg
}

func newCustomAnchorWalletFundedTestPackage(t *testing.T,
	backendInput uint32) *CustomAnchorTransferPackage {

	t.Helper()

	pkg := newCustomAnchorTestFixture(t).unsealed
	plans := make([]CustomAnchorInputSigningPlan, 0, len(pkg.SigningPlans)-1)
	for _, plan := range pkg.SigningPlans {
		if plan.InputIndex != backendInput {
			plans = append(plans, plan)
		}
	}
	require.Len(t, plans, len(pkg.SigningPlans)-1)
	pkg.SigningPlans = plans
	pkg.BackendManagedInputIndices = []uint32{backendInput}
	markCustomAnchorWalletFundedTestPackage(t, pkg)

	anchor := mustDecodeAnchorPSBT(t, pkg.CommittedAnchorPsbt)
	require.Less(t, backendInput, uint32(len(anchor.UnsignedTx.TxIn)))
	pkg.LockedUTXOs = []CustomAnchorLockedUTXO{{
		Outpoint: outpointFromWire(
			anchor.UnsignedTx.TxIn[backendInput].PreviousOutPoint,
		),
	}}

	return pkg
}

func markCustomAnchorWalletFundedTestPackage(t *testing.T,
	pkg *CustomAnchorTransferPackage) {

	t.Helper()
	anchor := mustDecodeAnchorPSBT(t, pkg.CommittedAnchorPsbt)
	actualFee, err := customAnchorTransactionFee(anchor)
	require.NoError(t, err)
	pkg.Funding = CustomAnchorFundingSummary{
		Mode:         CustomAnchorFundingWalletFunded,
		ActualFeeSat: actualFee,
		MaxFeeSat:    actualFee + 1,
	}
	pkg.FundingLock = CustomAnchorFundingLockMetadata{
		CustomLockID:          bytes.Repeat([]byte{1}, 32),
		LockExpirationSeconds: 600,
	}
}

//nolint:unparam // The packet index keeps this adversarial helper reusable.
func mutateCustomAnchorPackageVPacket(t *testing.T,
	pkg *CustomAnchorTransferPackage, role CustomAnchorPacketRole,
	packetIndex int, mutate func(*tappsbt.VPacket)) {

	t.Helper()

	var packets *[][]byte
	switch role {
	case CustomAnchorPacketRoleActive:
		packets = &pkg.ActiveVirtualPsbts

	case CustomAnchorPacketRolePassive:
		packets = &pkg.PassiveVirtualPsbts

	default:
		t.Fatalf("unsupported packet role %d", role)
	}

	require.Less(t, packetIndex, len(*packets))
	packet, err := tappsbt.Decode((*packets)[packetIndex])
	require.NoError(t, err)
	mutate(packet)
	(*packets)[packetIndex], err = tappsbt.Encode(packet)
	require.NoError(t, err)
}

func removeCustomAnchorTestField(unknowns []*btcpsbt.Unknown,
	key []byte) []*btcpsbt.Unknown {

	filtered := make([]*btcpsbt.Unknown, 0, len(unknowns))
	for _, unknown := range unknowns {
		if unknown != nil && bytes.Equal(unknown.Key, key) {
			continue
		}
		filtered = append(filtered, unknown)
	}

	return filtered
}

func mustDecodeAnchorPSBT(t *testing.T, raw []byte) *btcpsbt.Packet {
	t.Helper()
	packet, err := decodeAnchorPSBT(raw)
	require.NoError(t, err)
	return packet
}

func mustSerializeAnchorPSBT(t *testing.T,
	packet *btcpsbt.Packet) []byte {

	t.Helper()
	raw, err := serializeAnchorPSBT(packet)
	require.NoError(t, err)
	return raw
}
