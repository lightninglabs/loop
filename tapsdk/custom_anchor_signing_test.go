package tapsdk

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	btcpsbt "github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func TestCustomAnchorSigningRequests(t *testing.T) {
	fixture := newCustomAnchorTestFixture(t)
	pkg := fixture.sealed

	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	require.Len(t, requests.KeyPath, 1)
	require.Len(t, requests.MuSig2, 1)
	require.Len(t, requests.ScriptPath, 1)

	keyPath := requests.KeyPath[0]
	require.NotEmpty(t, keyPath.ID)
	require.Equal(t, pkg.CommittedPackageDigest, keyPath.PackageDigest)
	require.EqualValues(t, 0, keyPath.InputIndex)
	require.EqualValues(t, 330, keyPath.PrevOutValueSat)
	require.NotEmpty(t, keyPath.PrevOutScript)
	require.NotEmpty(t, keyPath.Sighash)
	require.Equal(t, keyPath.InternalKey, keyPath.Signer)

	muSig2 := requests.MuSig2[0]
	require.NotEmpty(t, muSig2.ID)
	require.NotEqual(t, keyPath.ID, muSig2.ID)
	require.EqualValues(t, 1, muSig2.InputIndex)
	require.Len(t, muSig2.Participants, 2)
	require.Equal(t, []byte("batch-session-1"), muSig2.SessionContext)

	scriptPath := requests.ScriptPath[0]
	require.NotEmpty(t, scriptPath.ID)
	require.NotEqual(t, keyPath.ID, scriptPath.ID)
	require.NotEqual(t, muSig2.ID, scriptPath.ID)
	require.EqualValues(t, 2, scriptPath.InputIndex)
	require.Equal(t, []byte{txscript.OP_TRUE}, scriptPath.LeafScript)
	require.NotEmpty(t, scriptPath.ControlBlock)
	require.NotEmpty(t, scriptPath.TaprootMerkleRoot)
	require.Empty(t, scriptPath.RequiredSigners)

	// Request slices are detached from both the package and future request
	// derivations.
	requests.MuSig2[0].Participants[0][0] ^= 1
	requests.MuSig2[0].SessionContext[0] ^= 1
	requests.ScriptPath[0].LeafScript[0] = txscript.OP_FALSE

	again, err := pkg.SigningRequests()
	require.NoError(t, err)
	require.Equal(
		t, fixture.sealed.SigningPlans[1].MuSig2.Participants,
		again.MuSig2[0].Participants,
	)
	require.Equal(t, []byte("batch-session-1"), again.MuSig2[0].SessionContext)
	require.Equal(t, []byte{txscript.OP_TRUE}, again.ScriptPath[0].LeafScript)
	require.Equal(t, keyPath.ID, again.KeyPath[0].ID)
}

func TestCustomAnchorSigningPlanValidation(t *testing.T) {
	validParticipant := customAnchorTestPubKey(
		customAnchorTestPrivateKey(8).PubKey(),
	)
	tests := []struct {
		name    string
		mutate  func(*CustomAnchorTransferPackage)
		wantErr string
	}{
		{
			name: "no variant",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans[0].KeyPath = nil
			},
			wantErr: "must set exactly one spending path",
		},
		{
			name: testMultipleVariants,
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans[0].ScriptPath =
					&CustomAnchorScriptPathSigningPlan{LeafHash: Hash{1}}
			},
			wantErr: "must set exactly one spending path",
		},
		{
			name: "duplicate input",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans[1].InputIndex = 0
			},
			wantErr: "duplicate signing plan for input 0",
		},
		{
			name: "input out of range",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans[0].InputIndex = 3
			},
			wantErr: "input index is out of range",
		},
		{
			name: "invalid key path signer",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans[0].KeyPath.Signer = XOnlyPubKey{}
			},
			wantErr: "invalid key-path signer",
		},
		{
			name: "too few musig participants",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans[1].MuSig2.Participants = []PubKey{
					validParticipant,
				}
			},
			wantErr: "requires at least two participants",
		},
		{
			name: "missing musig session context",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans[1].MuSig2.SessionContext = nil
			},
			wantErr: "MuSig2 session context is required",
		},
		{
			name: "duplicate musig participant",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				participant := pkg.SigningPlans[1].MuSig2.Participants[0]
				pkg.SigningPlans[1].MuSig2.Participants[1] = participant
			},
			wantErr: "duplicate MuSig2 participant",
		},
		{
			name: "missing script leaf",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans[2].ScriptPath.LeafHash = Hash{}
			},
			wantErr: "script-path leaf hash is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkg := newCustomAnchorTestFixture(t).unsealed
			test.mutate(pkg)

			_, err := pkg.Seal()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorSigningRequestMismatch(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CustomAnchorTransferPackage)
		wantErr string
		sealErr bool
	}{
		{
			name: "key-path signer",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans[0].KeyPath.Signer = customAnchorTestXOnly(
					customAnchorTestPrivateKey(10).PubKey(),
				)
			},
			wantErr: "key-path signer does not match",
		},
		{
			name: "musig aggregate",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans[1].MuSig2.Participants = []PubKey{
					customAnchorTestPubKey(
						customAnchorTestPrivateKey(11).PubKey(),
					),
					customAnchorTestPubKey(
						customAnchorTestPrivateKey(12).PubKey(),
					),
				}
			},
			wantErr: "participants do not match",
		},
		{
			name: "script leaf",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				pkg.SigningPlans[2].ScriptPath.LeafHash = Hash{99}
			},
			wantErr: "script-path leaf matched 0 PSBT entries",
		},
		{
			name: "missing previous output",
			mutate: func(pkg *CustomAnchorTransferPackage) {
				packet := mustDecodeAnchorPSBT(t, pkg.AnchorPsbt)
				packet.Inputs[0].WitnessUtxo = nil
				anchorPsbt := mustSerializeAnchorPSBT(t, packet)
				pkg.AnchorPsbt = anchorPsbt
				pkg.CommittedAnchorPsbt = cloneBytes(anchorPsbt)
			},
			wantErr: "previous output is required",
			sealErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkg := newCustomAnchorTestFixture(t).unsealed
			test.mutate(pkg)

			sealed, err := pkg.Seal()
			if test.sealErr {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			_, err = sealed.SigningRequests()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestApplyCustomAnchorKeyPathSignature(t *testing.T) {
	fixture := newCustomAnchorTestFixture(t)
	pkg := fixture.sealed
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	request := requests.KeyPath[0]

	tweakedPrivateKey := txscript.TweakTaprootPrivKey(
		*fixture.keyPathPrivateKey, request.TaprootMerkleRoot,
	)
	signature, err := schnorr.Sign(tweakedPrivateKey, request.Sighash[:])
	require.NoError(t, err)
	signatureBytes := signature.Serialize()
	originalSignature := append([]byte(nil), signatureBytes...)

	applied, err := pkg.ApplyKeyPathSignature(request.ID, signatureBytes)
	require.NoError(t, err)
	require.NotSame(t, pkg, applied)
	require.Equal(t, pkg.CommittedPackageDigest, applied.CommittedPackageDigest)
	require.Equal(t, pkg.UnsignedTxDigest, applied.UnsignedTxDigest)
	require.NotEqual(t, pkg.PackageDigest, applied.PackageDigest)
	require.NoError(t, applied.Validate())

	originalPacket := mustDecodeAnchorPSBT(t, pkg.AnchorPsbt)
	require.Empty(t, originalPacket.Inputs[0].TaprootKeySpendSig)
	appliedPacket := mustDecodeAnchorPSBT(t, applied.AnchorPsbt)
	require.Equal(
		t, originalSignature, appliedPacket.Inputs[0].TaprootKeySpendSig,
	)

	// The caller's signature buffer and request ID are not retained by
	// reference or changed after application.
	signatureBytes[0] ^= 1
	appliedPacket = mustDecodeAnchorPSBT(t, applied.AnchorPsbt)
	require.Equal(
		t, originalSignature, appliedPacket.Inputs[0].TaprootKeySpendSig,
	)
	afterRequests, err := applied.SigningRequests()
	require.NoError(t, err)
	require.Equal(t, request.ID, afterRequests.KeyPath[0].ID)

	idempotent, err := applied.ApplyKeyPathSignature(
		request.ID, originalSignature,
	)
	require.NoError(t, err)
	require.Equal(t, applied, idempotent)
}

func TestApplyCustomAnchorKeyPathSignatureErrors(t *testing.T) {
	fixture := newCustomAnchorTestFixture(t)
	pkg := fixture.sealed
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	request := requests.KeyPath[0]

	tests := []struct {
		name      string
		requestID Hash
		signature []byte
		wantErr   string
	}{
		{
			name:      "unknown request",
			requestID: Hash{99},
			signature: bytes.Repeat([]byte{1}, schnorr.SignatureSize),
			wantErr:   "unknown key-path signing request",
		},
		{
			name:      "short signature",
			requestID: request.ID,
			signature: []byte{1},
			wantErr:   "must be 64 or 65 bytes",
		},
		{
			name:      "invalid signature",
			requestID: request.ID,
			signature: bytes.Repeat([]byte{1}, schnorr.SignatureSize),
			wantErr:   "signature verification failed",
		},
		{
			name:      "default sighash suffix",
			requestID: request.ID,
			signature: bytes.Repeat(
				[]byte{1}, schnorr.SignatureSize+1,
			),
			wantErr: "default taproot sighash must not have a suffix",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := pkg.ApplyKeyPathSignature(
				test.requestID, test.signature,
			)
			require.Nil(t, result)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestVerifyTaprootKeyPathSignatureRequiresNonDefaultSuffix(t *testing.T) {
	t.Parallel()

	err := verifyTaprootKeyPathSignature(
		make([]byte, schnorr.SignatureSize),
		uint32(txscript.SigHashAll), Hash{}, nil,
	)
	require.ErrorContains(t, err, "non-default taproot sighash requires a suffix")
}

func TestApplyCustomAnchorScriptPathWitness(t *testing.T) {
	fixture := newCustomAnchorTestFixture(t)
	pkg := fixture.sealed
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	request := requests.ScriptPath[0]

	applied, err := pkg.ApplyScriptPathWitness(request.ID, nil)
	require.NoError(t, err)
	require.NotSame(t, pkg, applied)
	require.Equal(t, pkg.CommittedPackageDigest, applied.CommittedPackageDigest)
	require.Equal(t, pkg.UnsignedTxDigest, applied.UnsignedTxDigest)
	require.NotEqual(t, pkg.PackageDigest, applied.PackageDigest)
	require.NoError(t, applied.Validate())
	require.NoError(t, pkg.VerifyFinalAnchorPSBT(applied.AnchorPsbt))

	original := mustDecodeAnchorPSBT(t, pkg.AnchorPsbt)
	require.Empty(t, original.Inputs[2].FinalScriptWitness)
	final := mustDecodeAnchorPSBT(t, applied.AnchorPsbt)
	require.NotEmpty(t, final.Inputs[2].FinalScriptWitness)

	// Signing requests remain recoverable from the immutable committed
	// PSBT after standard finalization removes leaf metadata from the
	// current PSBT.
	afterRequests, err := applied.SigningRequests()
	require.NoError(t, err)
	require.Equal(t, request.ID, afterRequests.ScriptPath[0].ID)

	idempotent, err := applied.ApplyScriptPathWitness(request.ID, nil)
	require.NoError(t, err)
	require.Equal(t, applied, idempotent)
}

func TestApplyCustomAnchorScriptPathWitnessErrors(t *testing.T) {
	fixture := newCustomAnchorTestFixture(t)
	pkg := fixture.sealed
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	request := requests.ScriptPath[0]

	tests := []struct {
		name      string
		requestID Hash
		witness   [][]byte
		wantErr   string
	}{
		{
			name:      "unknown request",
			requestID: Hash{99},
			wantErr:   "unknown script-path signing request",
		},
		{
			name:      "invalid clean stack",
			requestID: request.ID,
			witness:   [][]byte{{1}},
			wantErr:   "script-path witness verification failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := pkg.ApplyScriptPathWitness(
				test.requestID, test.witness,
			)
			require.Nil(t, result)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorRequestIDBinding(t *testing.T) {
	fixture := newCustomAnchorTestFixture(t)
	requests, err := fixture.sealed.SigningRequests()
	require.NoError(t, err)

	changed := fixture.unsealed.Clone()
	changed.SigningPlans[1].MuSig2.SessionContext = []byte("another-session")
	changed, err = changed.Seal()
	require.NoError(t, err)
	changedRequests, err := changed.SigningRequests()
	require.NoError(t, err)

	require.NotEqual(
		t, fixture.sealed.CommittedPackageDigest,
		changed.CommittedPackageDigest,
	)
	require.NotEqual(t, requests.KeyPath[0].ID, changedRequests.KeyPath[0].ID)
	require.NotEqual(t, requests.MuSig2[0].ID, changedRequests.MuSig2[0].ID)
	require.NotEqual(
		t, requests.ScriptPath[0].ID, changedRequests.ScriptPath[0].ID,
	)
}

func TestVerifyFinalCustomAnchorPSBT(t *testing.T) {
	pkg := newCustomAnchorTestFixture(t).sealed
	require.NoError(t, pkg.VerifyFinalAnchorPSBT(pkg.AnchorPsbt))

	// PSBT signature and finalization fields may change without changing
	// the committed unsigned transaction.
	withSignature := mustDecodeAnchorPSBT(t, pkg.AnchorPsbt)
	withSignature.Inputs[0].TaprootKeySpendSig = bytes.Repeat(
		[]byte{1}, schnorr.SignatureSize,
	)
	require.NoError(t, pkg.VerifyFinalAnchorPSBT(
		mustSerializeAnchorPSBT(t, withSignature),
	))

	withFinalWitness := mustDecodeAnchorPSBT(t, pkg.AnchorPsbt)
	var witness bytes.Buffer
	require.NoError(t, btcpsbt.WriteTxWitness(&witness, [][]byte{{1}}))
	withFinalWitness.Inputs[0].FinalScriptWitness = witness.Bytes()
	require.NoError(t, pkg.VerifyFinalAnchorPSBT(
		mustSerializeAnchorPSBT(t, withFinalWitness),
	))

	tests := []struct {
		name    string
		mutate  func(*btcpsbt.Packet)
		wantErr string
	}{
		{
			name: testVersion,
			mutate: func(packet *btcpsbt.Packet) {
				packet.UnsignedTx.Version++
			},
			wantErr: "transaction version changed",
		},
		{
			name: testLocktime,
			mutate: func(packet *btcpsbt.Packet) {
				packet.UnsignedTx.LockTime++
			},
			wantErr: "transaction locktime changed",
		},
		{
			name: "input count",
			mutate: func(packet *btcpsbt.Packet) {
				packet.UnsignedTx.TxIn = packet.UnsignedTx.TxIn[:2]
				packet.Inputs = packet.Inputs[:2]
			},
			wantErr: "transaction input count changed",
		},
		{
			name: "input txid",
			mutate: func(packet *btcpsbt.Packet) {
				packet.UnsignedTx.TxIn[0].PreviousOutPoint.Hash =
					chainhash.Hash{99}
			},
			wantErr: "input 0 previous outpoint changed",
		},
		{
			name: "input index",
			mutate: func(packet *btcpsbt.Packet) {
				packet.UnsignedTx.TxIn[0].PreviousOutPoint.Index++
			},
			wantErr: "input 0 previous outpoint changed",
		},
		{
			name: "input sequence",
			mutate: func(packet *btcpsbt.Packet) {
				packet.UnsignedTx.TxIn[0].Sequence--
			},
			wantErr: "input 0 sequence changed",
		},
		{
			name: "output count",
			mutate: func(packet *btcpsbt.Packet) {
				packet.UnsignedTx.AddTxOut(&wire.TxOut{
					Value: 1, PkScript: []byte{txscript.OP_TRUE},
				})
				packet.Outputs = append(packet.Outputs, btcpsbt.POutput{})
			},
			wantErr: "transaction output count changed",
		},
		{
			name: "output value",
			mutate: func(packet *btcpsbt.Packet) {
				packet.UnsignedTx.TxOut[0].Value++
			},
			wantErr: "output 0 value changed",
		},
		{
			name: "output script",
			mutate: func(packet *btcpsbt.Packet) {
				packet.UnsignedTx.TxOut[0].PkScript = []byte{
					txscript.OP_FALSE,
				}
			},
			wantErr: "output 0 script changed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet := mustDecodeAnchorPSBT(t, pkg.AnchorPsbt)
			test.mutate(packet)

			err := pkg.VerifyFinalAnchorPSBT(
				mustSerializeAnchorPSBT(t, packet),
			)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorFinalSigningPlanEnforcement(t *testing.T) {
	pkg, fixture, secondKey := customAnchorSimpleSigningPackage(t)
	valid := finalizeCustomAnchorSigningPackage(
		t, pkg, fixture.keyPathPrivateKey, secondKey,
	)
	require.NoError(t, pkg.VerifyFinalAnchorPSBT(valid))
	_, err := pkg.WithFinalAnchorPSBT(valid)
	require.NoError(t, err)

	tests := []struct {
		name    string
		mutate  func(*btcpsbt.Packet)
		wantErr string
	}{
		{
			name: "script plan spent by key path",
			mutate: func(packet *btcpsbt.Packet) {
				committed := mustDecodeAnchorPSBT(
					t, pkg.CommittedAnchorPsbt,
				)
				packet.Inputs[2].TaprootMerkleRoot = cloneBytes(
					committed.Inputs[2].TaprootMerkleRoot,
				)
				setCustomAnchorTestKeyPathWitness(
					t, packet, 2, customAnchorTestPrivateKey(4),
					txscript.SigHashDefault,
				)
			},
			wantErr: "must use its sealed script path",
		},
		{
			name: "different sighash",
			mutate: func(packet *btcpsbt.Packet) {
				committed := mustDecodeAnchorPSBT(
					t, pkg.CommittedAnchorPsbt,
				)
				packet.Inputs[0].TaprootMerkleRoot = cloneBytes(
					committed.Inputs[0].TaprootMerkleRoot,
				)
				setCustomAnchorTestKeyPathWitness(
					t, packet, 0, fixture.keyPathPrivateKey,
					txscript.SigHashAll,
				)
			},
			wantErr: "default taproot sighash must not have a suffix",
		},
		{
			name: "external annex",
			mutate: func(packet *btcpsbt.Packet) {
				request := customAnchorScriptRequest(t, pkg)
				setCustomAnchorTestWitness(
					t, &packet.Inputs[2], wire.TxWitness{
						request.LeafScript, request.ControlBlock,
						{txscript.TaprootAnnexTag, 1},
					},
				)
			},
			wantErr: "annexes are unsupported",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet := mustDecodeAnchorPSBT(t, valid)
			test.mutate(packet)

			// Each adversarial witness remains valid under Bitcoin
			// consensus. The sealed plan is the additional boundary that
			// rejects it.
			finalTx, err := btcpsbt.Extract(packet)
			require.NoError(t, err)
			require.NoError(t, verifyFinalAnchorWitnesses(packet, finalTx))

			finalPSBT := mustSerializeAnchorPSBT(t, packet)
			err = pkg.VerifyFinalAnchorPSBT(finalPSBT)
			require.ErrorContains(t, err, test.wantErr)
			result, err := pkg.WithFinalAnchorPSBT(finalPSBT)
			require.Nil(t, result)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorFinalSigningPlanRejectsAlternateLeaf(t *testing.T) {
	selectedScript := []byte{txscript.OP_TRUE}
	alternateScript := []byte{txscript.OP_0, txscript.OP_NOT}
	pkg, fixture, secondKey, leaves := customAnchorScriptSigningPackage(
		t, selectedScript, alternateScript,
	)
	finalPSBT := finalizeCustomAnchorSigningPackage(
		t, pkg, fixture.keyPathPrivateKey, secondKey,
	)
	packet := mustDecodeAnchorPSBT(t, finalPSBT)
	setCustomAnchorTestWitness(
		t, &packet.Inputs[2], wire.TxWitness{
			leaves[1].Script, leaves[1].ControlBlock,
		},
	)

	finalTx, err := btcpsbt.Extract(packet)
	require.NoError(t, err)
	require.NoError(t, verifyFinalAnchorWitnesses(packet, finalTx))

	finalPSBT = mustSerializeAnchorPSBT(t, packet)
	err = pkg.VerifyFinalAnchorPSBT(finalPSBT)
	require.ErrorContains(t, err, "does not use the sealed script leaf")
	result, err := pkg.WithFinalAnchorPSBT(finalPSBT)
	require.Nil(t, result)
	require.ErrorContains(t, err, "does not use the sealed script leaf")
}

func TestCustomAnchorFinalSigningPlanRejectsScriptForKeyPlan(t *testing.T) {
	pkg, fixture, secondKey := customAnchorSimpleSigningPackage(t)
	unsealed := pkg.Clone()
	unsealed.SchemaVersion = 0
	unsealed.CommittedPackageDigest = Hash{}
	unsealed.UnsignedTxDigest = Hash{}
	unsealed.PackageDigest = Hash{}
	unsealed.SigningPlans[2] = CustomAnchorInputSigningPlan{
		InputIndex: 2,
		KeyPath: &CustomAnchorKeyPathSigningPlan{
			Signer: customAnchorTestXOnly(
				customAnchorTestPrivateKey(4).PubKey(),
			),
		},
	}
	keyPathPackage, err := unsealed.Seal()
	require.NoError(t, err)
	finalPSBT := finalizeCustomAnchorSigningPackageWithoutRequests(
		t, keyPathPackage, fixture.keyPathPrivateKey, secondKey,
	)
	packet := mustDecodeAnchorPSBT(t, finalPSBT)
	finalTx, err := btcpsbt.Extract(packet)
	require.NoError(t, err)
	require.NoError(t, verifyFinalAnchorWitnesses(packet, finalTx))

	err = keyPathPackage.VerifyFinalAnchorPSBT(finalPSBT)
	require.ErrorContains(t, err, "must use its sealed key path")
	result, err := keyPathPackage.WithFinalAnchorPSBT(finalPSBT)
	require.Nil(t, result)
	require.ErrorContains(t, err, "must use its sealed key path")
}

func TestCustomAnchorFinalScriptPathSighashEnforcement(t *testing.T) {
	scriptKey := customAnchorTestPrivateKey(10)
	script, err := txscript.NewScriptBuilder().AddData(
		schnorr.SerializePubKey(scriptKey.PubKey()),
	).AddOp(txscript.OP_CHECKSIG).Script()
	require.NoError(t, err)
	pkg, fixture, secondKey, _ := customAnchorScriptSigningPackage(t, script)
	undeclaredSigner := finalizeCustomAnchorScriptSignaturePackage(
		t, pkg, fixture.keyPathPrivateKey, secondKey, scriptKey,
		txscript.SigHashDefault,
	)
	result, err := pkg.WithFinalAnchorPSBT(undeclaredSigner)
	require.Nil(t, result)
	require.ErrorContains(t, err, "does not match a declared signer")

	unsealed := pkg.Clone()
	unsealed.SchemaVersion = 0
	unsealed.CommittedPackageDigest = Hash{}
	unsealed.UnsignedTxDigest = Hash{}
	unsealed.PackageDigest = Hash{}
	unsealed.SigningPlans[2].ScriptPath.RequiredSigners = []XOnlyPubKey{
		customAnchorTestXOnly(scriptKey.PubKey()),
	}
	pkg, err = unsealed.Seal()
	require.NoError(t, err)

	valid := finalizeCustomAnchorScriptSignaturePackage(
		t, pkg, fixture.keyPathPrivateKey, secondKey, scriptKey,
		txscript.SigHashDefault,
	)
	_, err = pkg.WithFinalAnchorPSBT(valid)
	require.NoError(t, err)

	differentSighash := finalizeCustomAnchorScriptSignaturePackage(
		t, pkg, fixture.keyPathPrivateKey, secondKey, scriptKey,
		txscript.SigHashAll,
	)
	packet := mustDecodeAnchorPSBT(t, differentSighash)
	finalTx, err := btcpsbt.Extract(packet)
	require.NoError(t, err)
	require.NoError(t, verifyFinalAnchorWitnesses(packet, finalTx))
	err = pkg.VerifyFinalAnchorPSBT(differentSighash)
	require.ErrorContains(t, err, "exact sealed sighash")
	result, err = pkg.WithFinalAnchorPSBT(differentSighash)
	require.Nil(t, result)
	require.ErrorContains(t, err, "exact sealed sighash")
}

func TestCustomAnchorBackendManagedSigningRemainsConsensusOnly(t *testing.T) {
	pkg, fixture, secondKey := customAnchorSimpleSigningPackage(t)
	unsealed := pkg.Clone()
	unsealed.SchemaVersion = 0
	unsealed.CommittedPackageDigest = Hash{}
	unsealed.UnsignedTxDigest = Hash{}
	unsealed.PackageDigest = Hash{}
	unsealed.SigningPlans = unsealed.SigningPlans[:2]
	unsealed.BackendManagedInputIndices = []uint32{2}
	markCustomAnchorWalletFundedTestPackage(t, unsealed)
	anchor := mustDecodeAnchorPSBT(t, unsealed.CommittedAnchorPsbt)
	unsealed.LockedUTXOs = []CustomAnchorLockedUTXO{{
		Outpoint: outpointFromWire(
			anchor.UnsignedTx.TxIn[2].PreviousOutPoint,
		),
	}}

	backendPackage, err := unsealed.Seal()
	require.NoError(t, err)
	finalPSBT := finalizeCustomAnchorSigningPackageWithoutRequests(
		t, backendPackage, fixture.keyPathPrivateKey, secondKey,
	)
	packet := mustDecodeAnchorPSBT(t, finalPSBT)
	committed := mustDecodeAnchorPSBT(t, backendPackage.CommittedAnchorPsbt)
	leaf := committed.Inputs[2].TaprootLeafScript[0]
	setCustomAnchorTestWitness(
		t, &packet.Inputs[2], wire.TxWitness{
			leaf.Script, leaf.ControlBlock,
			{txscript.TaprootAnnexTag, 1},
		},
	)
	finalPSBT = mustSerializeAnchorPSBT(t, packet)

	require.NoError(t, backendPackage.VerifyFinalAnchorPSBT(finalPSBT))
	_, err = backendPackage.WithFinalAnchorPSBT(finalPSBT)
	require.NoError(t, err)
}

func TestCustomAnchorCodeSeparatorRejected(t *testing.T) {
	codeSeparatorScript := []byte{
		txscript.OP_CODESEPARATOR, txscript.OP_TRUE,
	}
	pkg, fixture, secondKey, _ := customAnchorScriptSigningPackage(
		t, codeSeparatorScript,
	)

	_, err := pkg.SigningRequests()
	require.ErrorContains(t, err, "OP_CODESEPARATOR is unsupported")
	result, err := pkg.ApplyScriptPathWitness(Hash{}, nil)
	require.Nil(t, result)
	require.ErrorContains(t, err, "OP_CODESEPARATOR is unsupported")

	finalPSBT := finalizeCustomAnchorSigningPackageWithoutRequests(
		t, pkg, fixture.keyPathPrivateKey, secondKey,
	)
	finalPacket := mustDecodeAnchorPSBT(t, finalPSBT)
	finalTx, err := btcpsbt.Extract(finalPacket)
	require.NoError(t, err)
	require.NoError(t, verifyFinalAnchorWitnesses(finalPacket, finalTx))
	err = pkg.VerifyFinalAnchorPSBT(finalPSBT)
	require.ErrorContains(t, err, "OP_CODESEPARATOR is unsupported")
	result, err = pkg.WithFinalAnchorPSBT(finalPSBT)
	require.Nil(t, result)
	require.ErrorContains(t, err, "OP_CODESEPARATOR is unsupported")

	// A pushed byte with the same value is data, not an opcode, and must not
	// be rejected by the tokenizer-based check.
	pushedCodeSeparator, err := txscript.NewScriptBuilder().
		AddData([]byte{txscript.OP_CODESEPARATOR}).
		AddOp(txscript.OP_DROP).AddOp(txscript.OP_TRUE).Script()
	require.NoError(t, err)
	pushedPackage, _, _, _ := customAnchorScriptSigningPackage(
		t, pushedCodeSeparator,
	)
	_, err = pushedPackage.SigningRequests()
	require.NoError(t, err)
}

func customAnchorSimpleSigningPackage(t *testing.T) (
	*CustomAnchorTransferPackage, *customAnchorTestFixture,
	*btcec.PrivateKey) {

	t.Helper()

	pkg, fixture, secondKey, _ := customAnchorScriptSigningPackage(
		t, []byte{txscript.OP_TRUE},
	)

	return pkg, fixture, secondKey
}

func customAnchorScriptSigningPackage(t *testing.T, scripts ...[]byte) (
	*CustomAnchorTransferPackage, *customAnchorTestFixture,
	*btcec.PrivateKey, []*btcpsbt.TaprootTapLeafScript) {

	t.Helper()
	require.NotEmpty(t, scripts)

	fixture := newCustomAnchorTestFixture(t)
	unsealed := fixture.unsealed.Clone()
	packet := mustDecodeAnchorPSBT(t, unsealed.AnchorPsbt)

	secondKey := customAnchorTestPrivateKey(9)
	secondOutputKey := txscript.ComputeTaprootKeyNoScript(secondKey.PubKey())
	secondScript, err := txscript.PayToTaprootScript(secondOutputKey)
	require.NoError(t, err)
	packet.Inputs[1] = btcpsbt.PInput{
		WitnessUtxo: &wire.TxOut{
			Value:    packet.Inputs[1].WitnessUtxo.Value,
			PkScript: secondScript,
		},
		TaprootInternalKey: schnorr.SerializePubKey(secondKey.PubKey()),
	}
	unsealed.SigningPlans[1] = CustomAnchorInputSigningPlan{
		InputIndex: 1,
		KeyPath: &CustomAnchorKeyPathSigningPlan{
			Signer: customAnchorTestXOnly(secondKey.PubKey()),
		},
	}

	leaves := make([]txscript.TapLeaf, len(scripts))
	for idx := range scripts {
		leaves[idx] = txscript.NewBaseTapLeaf(scripts[idx])
	}
	tree := txscript.AssembleTaprootScriptTree(leaves...)
	root := tree.RootNode.TapHash()
	scriptKey := customAnchorTestPrivateKey(4)
	outputKey := txscript.ComputeTaprootOutputKey(scriptKey.PubKey(), root[:])
	pkScript, err := txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)

	leafScripts := make([]*btcpsbt.TaprootTapLeafScript, len(leaves))
	for idx := range leaves {
		controlBlock := tree.LeafMerkleProofs[idx].ToControlBlock(
			scriptKey.PubKey(),
		)
		controlBlockBytes, err := controlBlock.ToBytes()
		require.NoError(t, err)
		leafScripts[idx] = &btcpsbt.TaprootTapLeafScript{
			ControlBlock: controlBlockBytes,
			Script:       cloneBytes(leaves[idx].Script),
			LeafVersion:  leaves[idx].LeafVersion,
		}
	}
	packet.Inputs[2] = btcpsbt.PInput{
		WitnessUtxo: &wire.TxOut{
			Value:    packet.Inputs[2].WitnessUtxo.Value,
			PkScript: pkScript,
		},
		TaprootInternalKey: schnorr.SerializePubKey(scriptKey.PubKey()),
		TaprootMerkleRoot:  cloneBytes(root[:]),
		TaprootLeafScript:  leafScripts,
	}
	selectedHash := leaves[0].TapHash()
	unsealed.SigningPlans[2].ScriptPath.LeafHash = Hash(selectedHash)

	unsealed.AnchorPsbt = mustSerializeAnchorPSBT(t, packet)
	unsealed.CommittedAnchorPsbt = nil
	sealed, err := unsealed.Seal()
	require.NoError(t, err)

	return sealed, fixture, secondKey, leafScripts
}

func finalizeCustomAnchorSigningPackage(t *testing.T,
	pkg *CustomAnchorTransferPackage, firstKey,
	secondKey *btcec.PrivateKey) []byte {

	t.Helper()
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	require.Len(t, requests.ScriptPath, 1)

	packet := mustDecodeAnchorPSBT(t, pkg.CommittedAnchorPsbt)
	setCustomAnchorTestKeyPathWitness(
		t, packet, 0, firstKey, packet.Inputs[0].SighashType,
	)
	setCustomAnchorTestKeyPathWitness(
		t, packet, 1, secondKey, packet.Inputs[1].SighashType,
	)
	setCustomAnchorTestWitness(
		t, &packet.Inputs[2], wire.TxWitness{
			requests.ScriptPath[0].LeafScript,
			requests.ScriptPath[0].ControlBlock,
		},
	)

	return mustSerializeAnchorPSBT(t, packet)
}

func finalizeCustomAnchorSigningPackageWithoutRequests(t *testing.T,
	pkg *CustomAnchorTransferPackage, firstKey,
	secondKey *btcec.PrivateKey) []byte {

	t.Helper()
	packet := mustDecodeAnchorPSBT(t, pkg.CommittedAnchorPsbt)
	setCustomAnchorTestKeyPathWitness(
		t, packet, 0, firstKey, packet.Inputs[0].SighashType,
	)
	setCustomAnchorTestKeyPathWitness(
		t, packet, 1, secondKey, packet.Inputs[1].SighashType,
	)
	leaf := packet.Inputs[2].TaprootLeafScript[0]
	setCustomAnchorTestWitness(
		t, &packet.Inputs[2], wire.TxWitness{
			leaf.Script, leaf.ControlBlock,
		},
	)

	return mustSerializeAnchorPSBT(t, packet)
}

func finalizeCustomAnchorScriptSignaturePackage(t *testing.T,
	pkg *CustomAnchorTransferPackage, firstKey, secondKey,
	scriptKey *btcec.PrivateKey, scriptSighash txscript.SigHashType) []byte {

	t.Helper()
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	require.Len(t, requests.ScriptPath, 1)
	request := requests.ScriptPath[0]

	packet := mustDecodeAnchorPSBT(t, pkg.CommittedAnchorPsbt)
	setCustomAnchorTestKeyPathWitness(
		t, packet, 0, firstKey, packet.Inputs[0].SighashType,
	)
	setCustomAnchorTestKeyPathWitness(
		t, packet, 1, secondKey, packet.Inputs[1].SighashType,
	)
	prevOutFetcher, prevOuts, err := anchorPrevOuts(packet)
	require.NoError(t, err)
	sigHashes := txscript.NewTxSigHashes(packet.UnsignedTx, prevOutFetcher)
	signature, err := txscript.RawTxInTapscriptSignature(
		packet.UnsignedTx, sigHashes, 2, prevOuts[2].Value,
		prevOuts[2].PkScript, txscript.NewTapLeaf(
			txscript.TapscriptLeafVersion(request.LeafVersion),
			request.LeafScript,
		), scriptSighash, scriptKey,
	)
	require.NoError(t, err)
	setCustomAnchorTestWitness(
		t, &packet.Inputs[2], wire.TxWitness{
			signature, request.LeafScript, request.ControlBlock,
		},
	)

	return mustSerializeAnchorPSBT(t, packet)
}

func setCustomAnchorTestKeyPathWitness(t *testing.T,
	packet *btcpsbt.Packet, inputIndex int, privateKey *btcec.PrivateKey,
	sighashType txscript.SigHashType) {

	t.Helper()
	prevOutFetcher, prevOuts, err := anchorPrevOuts(packet)
	require.NoError(t, err)
	sigHashes := txscript.NewTxSigHashes(packet.UnsignedTx, prevOutFetcher)
	signature, err := txscript.RawTxInTaprootSignature(
		packet.UnsignedTx, sigHashes, inputIndex,
		prevOuts[inputIndex].Value, prevOuts[inputIndex].PkScript,
		packet.Inputs[inputIndex].TaprootMerkleRoot, sighashType,
		privateKey,
	)
	require.NoError(t, err)
	setCustomAnchorTestWitness(
		t, &packet.Inputs[inputIndex], wire.TxWitness{signature},
	)
}

func setCustomAnchorTestWitness(t *testing.T, input *btcpsbt.PInput,
	witness wire.TxWitness) {

	t.Helper()
	var serialized bytes.Buffer
	require.NoError(t, btcpsbt.WriteTxWitness(&serialized, witness))
	input.FinalScriptWitness = serialized.Bytes()
}

func customAnchorScriptRequest(t *testing.T,
	pkg *CustomAnchorTransferPackage) CustomAnchorScriptPathSigningRequest {

	t.Helper()
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	require.Len(t, requests.ScriptPath, 1)

	return requests.ScriptPath[0]
}
