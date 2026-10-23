//go:build itest

package itest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sort"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit"
	loopsdk "github.com/lightninglabs/loop/swapkit/tapsdk"
	tapsdk "github.com/lightninglabs/tap-sdk"
	sdkharness "github.com/lightninglabs/tap-sdk/itest"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

// TestSwapKitDepositLifecycle exercises asset conservation and proof delivery
// through a static deposit, a cooperative transition to an HTLC, and either a
// preimage claim or a unilateral refund. Authorization stays on the Bitcoin
// anchor; each asset spend regenerates its own OP_TRUE witness. Lightning
// invoice settlement and server execution policy are outside this Loop
// contract test.
func TestSwapKitDepositLifecycle(t *testing.T) {
	for _, outcome := range []string{
		"success", "htlc_timeout", "deposit_timeout",
	} {
		t.Run(outcome, func(t *testing.T) {
			testSwapKitDepositLifecycle(t, outcome)
		})
	}
}

func testSwapKitDepositLifecycle(t *testing.T, outcome string) {
	h := sdkharness.NewTestHarnessWithTransport(t, sdkharness.TransportGRPC)
	ctx := t.Context()
	h.FundLndWallet(t, ctx)
	h.EnableUniverseBootstrap(t, ctx)

	name := uniqueEventLabel("swapkit-" + outcome)
	minted, err := h.CreateFungibleAndConfirm(t, ctx, name, 1_000)
	require.NoError(t, err)
	require.True(t, minted.Ref.IsGroupRef())
	mintProof, err := h.AliceWallet.ExportProofFile(
		ctx,
		tapsdk.AssetRefFromAssetID(minted.Asset.Genesis.IssuanceID),
		minted.Asset.ScriptKey.PubKey, nil,
	)
	require.NoError(t, err)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientPub := mustCompressedPubKey(t, clientKey.PubKey())
	serverPub := mustCompressedPubKey(t, serverKey.PubKey())
	deposit, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: clientPub,
		ServerKey: serverPub,
		CSVExpiry: 18,
	})
	require.NoError(t, err)

	// The initial asset input remains wallet signed. Both subsequent asset
	// outputs use fresh OP_TRUE keys, with the policy on their BTC anchors.
	anchor, value := swapKitAnchorPSBT(
		t, mintProof,
		loopsdk.AnchorPlan(deposit.AnchorPlan()).InternalKey, nil,
	)
	depositOutput := swapKitOutput(
		t, "deposit", minted.Ref, minted.Asset.Amount, value,
		deposit.AnchorPlan(),
	)
	mintAnchorKey := swapKitLastProof(t, mintProof).
		InclusionProof.InternalKey
	request := swapKitRequest(
		tapsdk.CustomAssetInput{
			ID: "mint", AssetRef: minted.Ref,
			Amount:    minted.Asset.Amount,
			ProofFile: mintProof.RawProofFile,
			Witness: tapsdk.CustomAssetWitnessPlan{
				Mode: tapsdk.CustomAssetWitnessBackendSigner,
			},
		}, depositOutput, anchor,
		tapsdk.CustomAnchorInputSigningPlan{
			InputIndex: 0,
			KeyPath: &tapsdk.CustomAnchorKeyPathSigningPlan{
				Signer: mustXOnlyPubKey(t, mintAnchorKey),
			},
		},
	)
	sealed := swapKitCommit(t, ctx, h, name, request)
	final, err := newAliceLndAnchorSigner(t)(ctx, sealed.AnchorPsbt)
	require.NoError(t, err)
	output, inputProof := swapKitPublish(
		t, ctx, h, sealed, final,
	)
	require.NotNil(t, output.OPTrueSpend)
	require.NoError(t, output.OPTrueSpend.Validate(output.ScriptKey))

	var (
		spend          *swapkit.SwapAnchorSpendInfo
		witness        func([]byte) ([][]byte, error)
		signer         = clientKey
		receiverWallet = h.AliceWallet
	)
	if outcome == "deposit_timeout" {
		spend, err = deposit.TimeoutSpend(output.TaprootAssetRoot)
		require.NoError(t, err)
		witness = func(sig []byte) ([][]byte, error) {
			return deposit.TimeoutWitness(
				output.TaprootAssetRoot, sig,
			)
		}
		h.MineBlocks(t, int(deposit.Params().CSVExpiry))
		h.WaitForSync(t, ctx, h.AliceClient, defaultSyncTimeout)
	} else {
		info, err := h.AliceClient.GetInfo(ctx)
		require.NoError(t, err)
		preimage := sha256.Sum256([]byte(name))
		htlc, err := swapkit.NewSwapKit(swapkit.SwapKitParams{
			Policy:      swapkit.SwapHTLCStaticLoopInV3,
			SenderKey:   clientPub,
			ReceiverKey: serverPub,
			SenderInternalKey: mustCompressedPubKey(
				t, clientKey.PubKey(),
			),
			ReceiverInternalKey: mustCompressedPubKey(
				t, serverKey.PubKey(),
			),
			PaymentHash: tapsdk.Hash(sha256.Sum256(preimage[:])),
			Expiry:      info.BlockHeight + 40,
		})
		require.NoError(t, err)

		anchor, value = swapKitAnchorPSBT(
			t, inputProof,
			loopsdk.AnchorPlan(htlc.AnchorPlan()).InternalKey, nil,
		)
		packet := decodeCustomAnchorITestPSBT(t, anchor)
		depositKey := deposit.AnchorPlan().InternalKey
		packet.Inputs[0].TaprootInternalKey = depositKey[1:]
		packet.Inputs[0].TaprootMerkleRoot = output.TaprootMerkleRoot[:]
		anchor = swapKitSerializePSBT(t, packet)
		htlcOutput := swapKitOutput(
			t, "htlc", minted.Ref, minted.Asset.Amount, value,
			htlc.AnchorPlan(),
		)
		request = swapKitRequest(
			swapKitInput(output, inputProof), htlcOutput, anchor,
			loopsdk.CooperativeSigningPlan(
				deposit.CooperativeParticipants(), 0, []byte(name),
			),
		)
		sealed = swapKitCommit(t, ctx, h, name+"-htlc", request)
		signing, err := sealed.SigningRequests()
		require.NoError(t, err)
		require.Empty(t, signing.KeyPath)
		require.Empty(t, signing.ScriptPath)
		require.Len(t, signing.MuSig2, 1)
		muSig2 := signing.MuSig2[0]

		// The kit sorts participants before aggregating. Use that same
		// order when conducting the cooperative signing ceremony.
		signers := []*btcec.PrivateKey{clientKey, serverKey}
		sort.Slice(signers, func(i, j int) bool {
			return bytes.Compare(
				signers[i].PubKey().SerializeCompressed(),
				signers[j].PubKey().SerializeCompressed(),
			) < 0
		})
		participants := []*btcec.PublicKey{
			signers[0].PubKey(), signers[1].PubKey(),
		}
		signature := customAnchorMuSig2Sign(
			t, signers, participants, muSig2.Sighash,
			muSig2.TaprootMerkleRoot,
		)
		if muSig2.SighashType != uint32(txscript.SigHashDefault) {
			signature = append(signature, byte(muSig2.SighashType))
		}
		sealed, err = sealed.ApplyKeyPathSignature(muSig2.ID, signature)
		require.NoError(t, err)
		final = customAnchorFinalizeKeyPath(t, sealed.AnchorPsbt)
		previousScriptKey := output.ScriptKey
		output, inputProof = swapKitPublish(t, ctx, h, sealed, final)
		require.NotEqual(t, previousScriptKey, output.ScriptKey)
		require.NotNil(t, output.OPTrueSpend)
		require.NoError(t, output.OPTrueSpend.Validate(
			output.ScriptKey,
		))

		if outcome == "success" {
			signer, receiverWallet = serverKey, h.BobWallet
			spend, err = htlc.SuccessSpend(output.TaprootAssetRoot)
			require.NoError(t, err)
			witness = func(sig []byte) ([][]byte, error) {
				return htlc.SuccessWitness(
					output.TaprootAssetRoot, sig, preimage,
				)
			}
		} else {
			spend, err = htlc.TimeoutSpend(output.TaprootAssetRoot)
			require.NoError(t, err)
			witness = func(sig []byte) ([][]byte, error) {
				return htlc.TimeoutWitness(
					output.TaprootAssetRoot, sig,
				)
			}
			info, err = h.AliceClient.GetInfo(ctx)
			require.NoError(t, err)
			if info.BlockHeight <= htlc.Params().Expiry {
				h.MineBlocks(t, int(htlc.Params().Expiry-
					info.BlockHeight+1))
			}
			h.WaitForSync(t, ctx, h.AliceClient, defaultSyncTimeout)
		}
	}

	// Claim or refund to a normal wallet-owned asset output. The SDK checks
	// the anchor witness against the real committed prevout before publish.
	receiverKeys, err := receiverWallet.DeriveKeys(ctx)
	require.NoError(t, err)
	require.Equal(t, [32]byte(output.TaprootMerkleRoot),
		spend.TaprootMerkleRoot)
	anchor, value = swapKitAnchorPSBT(
		t, inputProof, receiverKeys.InternalKey, spend,
	)
	request = swapKitRequest(
		swapKitInput(output, inputProof), tapsdk.CustomAssetOutput{
			ID: "recipient", AssetRef: minted.Ref,
			Amount: minted.Asset.Amount, AnchorOutputIndex: 0,
			AnchorValueSat: uint64(value),
			Script: tapsdk.CustomAssetScriptPlan{
				Mode: tapsdk.CustomAssetScriptExternal,
				External: &tapsdk.CustomAssetExternalScriptPlan{
					ScriptKey: receiverKeys.ScriptKey,
				},
			},
			Anchor: tapsdk.CustomAnchorOutputPlan{
				InternalKey: receiverKeys.InternalKey,
			},
		}, anchor, loopsdk.ScriptSigningPlan(spend, 0),
	)
	sealed = swapKitCommit(t, ctx, h, name+"-claim", request)
	signing, err := sealed.SigningRequests()
	require.NoError(t, err)
	require.Empty(t, signing.KeyPath)
	require.Empty(t, signing.MuSig2)
	require.Len(t, signing.ScriptPath, 1)
	scriptRequest := signing.ScriptPath[0]
	sig, err := schnorr.Sign(signer, scriptRequest.Sighash[:])
	require.NoError(t, err)
	signature := sig.Serialize()
	if scriptRequest.SighashType != uint32(txscript.SigHashDefault) {
		signature = append(signature, byte(scriptRequest.SighashType))
	}
	stack, err := witness(signature)
	require.NoError(t, err)
	require.Equal(t, scriptRequest.LeafScript, stack[len(stack)-2])
	require.Equal(t, scriptRequest.ControlBlock, stack[len(stack)-1])
	sealed, err = sealed.ApplyScriptPathWitness(
		scriptRequest.ID, stack[:len(stack)-2],
	)
	require.NoError(t, err)
	output, finalProof := swapKitPublish(
		t, ctx, h, sealed, sealed.AnchorPsbt,
	)
	h.WaitForSync(t, ctx, h.BobClient, defaultSyncTimeout)
	registered, err := receiverWallet.ImportProofFile(ctx, finalProof)
	require.NoError(t, err)
	require.Equal(t, minted.Asset.Genesis.IssuanceID, registered.IssuanceID)
	require.Equal(t, minted.Asset.Amount, registered.Amount)
	require.Equal(t, output.ScriptKey, registered.ScriptKey)
	balance := h.WaitForBalance(
		t, ctx, receiverWallet, minted.Ref, minted.Asset.Amount,
		balanceTimeoutFor(minted.Ref),
	)
	require.Equal(t, minted.Asset.Amount, balance)
}

// swapKitOutput explicitly selects the SDK supplied-key/asset-ID OP_TRUE tree.
// This preserves funded-descriptor compatibility coverage; normal integration
// defaults to fixed NUMS, exercised by the native Nautilus consumer fixture.
func swapKitOutput(t testing.TB, id string, ref tapsdk.AssetRef,
	amount uint64, value int64,
	anchor swapkit.AnchorPlan) tapsdk.CustomAssetOutput {

	t.Helper()
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	return loopsdk.OPTrueOutput(tapsdk.CustomAssetOutput{
		ID: id, AssetRef: ref, Amount: amount,
		AnchorOutputIndex: 0, AnchorValueSat: uint64(value),
	}, tapsdk.KeyDescriptor{
		RawKeyBytes: mustCompressedPubKey(t, key.PubKey()),
	}, anchor)
}

// swapKitInput carries forward the exact proof and OP_TRUE witness.
func swapKitInput(output tapsdk.CustomAnchorAssetOutputSummary,
	inputProof *tapsdk.ProofFile) tapsdk.CustomAssetInput {

	return tapsdk.CustomAssetInput{
		ID: output.LogicalOutputID, AssetRef: output.AssetRef,
		Amount: output.Amount, ProofFile: inputProof.RawProofFile,
		Witness: tapsdk.CustomAssetWitnessPlan{
			Mode:  tapsdk.CustomAssetWitnessCallerProvided,
			Stack: output.OPTrueSpend.WitnessStack(),
		},
	}
}

// swapKitRequest requires exact funding without burns or passive assets.
func swapKitRequest(
	input tapsdk.CustomAssetInput,
	output tapsdk.CustomAssetOutput,
	anchor []byte,
	signing tapsdk.CustomAnchorInputSigningPlan,
) *tapsdk.CustomAnchorRequest {

	funding := tapsdk.CustomAnchorFundingPlan{
		Mode: tapsdk.CustomAnchorFundingCallerFundedExact,
	}
	funding.CallerFundedExact = &tapsdk.CustomAnchorCallerFundedExact{}
	return &tapsdk.CustomAnchorRequest{
		Inputs:  []tapsdk.CustomAssetInput{input},
		Outputs: []tapsdk.CustomAssetOutput{output}, AnchorPSBT: anchor,
		Funding: funding,
		PassiveAssets: tapsdk.CustomAnchorPassiveAssets{
			Policy: tapsdk.CustomAnchorPassiveReject,
		},
		LossPolicy: tapsdk.CustomAnchorLossPolicy{
			Mode: tapsdk.CustomAnchorLossReject,
		},
		SigningPlans: []tapsdk.CustomAnchorInputSigningPlan{signing},
	}
}

// swapKitCommit verifies the plan before creating its transfer package.
func swapKitCommit(
	t testing.TB,
	ctx context.Context,
	h *sdkharness.TestHarness,
	label string,
	request *tapsdk.CustomAnchorRequest,
) *tapsdk.CustomAnchorTransferPackage {

	t.Helper()
	// Confirmed proof selection does not populate the publishing tapd's
	// archive. Import each selected input before commit so its confirmation
	// handler can reconstruct the successor proof from durable state.
	for _, input := range request.Inputs {
		_, err := h.AliceWallet.ImportProofFile(ctx, &tapsdk.ProofFile{
			RawProofFile: input.ProofFile,
		})
		require.NoError(t, err)
	}
	plan, err := h.AliceWallet.NewCustomAnchorTxBuilder().Build(
		ctx, request,
	)
	require.NoError(t, err)
	verification := plan.Verification()
	require.True(t, verification.Valid())
	sealed, err := plan.Commit(ctx, tapsdk.CustomAnchorCommitOptions{
		Publish: tapsdk.CustomAnchorPublishMetadata{Label: label},
	})
	require.NoError(t, err)
	require.NoError(t, sealed.Validate())
	require.Len(t, sealed.Outputs, 1)
	return sealed
}

// swapKitPublish confirms the anchor and obtains its complete asset proof.
func swapKitPublish(t testing.TB, ctx context.Context,
	h *sdkharness.TestHarness, sealed *tapsdk.CustomAnchorTransferPackage,
	final []byte) (
	tapsdk.CustomAnchorAssetOutputSummary, *tapsdk.ProofFile) {

	t.Helper()
	require.NoError(t, sealed.VerifyFinalAnchorPSBT(final))
	_, err := h.AliceWallet.PublishCustomAnchorTransfer(ctx, sealed, final)
	require.NoError(t, err)
	h.MineBlocks(t, defaultMineBlocks)
	h.WaitForSync(t, ctx, h.AliceClient, defaultSyncTimeout)
	output := sealed.Outputs[0]
	if output.OPTrueSpend != nil {
		// RegisterTransfer only archives declared publisher keys.
		// This declares an external script, not a wallet signing key.
		control, err := txscript.ParseControlBlock(
			output.OPTrueSpend.ControlBlock,
		)
		require.NoError(t, err)
		_, err = h.AliceClient.DeclareScriptKey(
			ctx, &tapsdk.DeclareScriptKeyRequest{
				ScriptKey: tapsdk.ScriptKey{
					PubKey: output.ScriptKey,
					KeyDesc: tapsdk.KeyDescriptor{
						RawKeyBytes: output.OPTrueSpend.
							InternalKey,
					},
					TapTweak: control.RootHash(
						output.OPTrueSpend.LeafScript,
					),
				},
			},
		)
		require.NoError(t, err)
	}
	file := h.WaitForProofFile(
		t, ctx, h.AliceWallet,
		tapsdk.AssetRefFromAssetID(output.IssuanceID), output.ScriptKey,
		&output.AnchorOutpoint,
	)
	require.NotEmpty(t, file.RawProofFile)
	return output, file
}

// swapKitLastProof decodes the actual anchor prevout for PSBT construction.
func swapKitLastProof(t testing.TB, file *tapsdk.ProofFile) *proof.Proof {
	t.Helper()
	decoded, err := proof.DecodeFile(file.RawProofFile)
	require.NoError(t, err)
	last, err := decoded.LastProof()
	require.NoError(t, err)
	return last
}

// swapKitAnchorPSBT leaves enough of the mint anchor for all three transfers.
// Each transaction pays 200 sat, retaining a non-dust final output.
func swapKitAnchorPSBT(t testing.TB, file *tapsdk.ProofFile,
	receiver tapsdk.InternalKey, spend *swapkit.SwapAnchorSpendInfo) (
	[]byte, int64) {

	t.Helper()
	inputProof := swapKitLastProof(t, file)
	outpoint := inputProof.OutPoint()
	prevout := inputProof.AnchorTx.TxOut[outpoint.Index]
	value := prevout.Value - 200
	require.GreaterOrEqual(t, value, int64(330))
	internalKey, err := btcec.ParsePubKey(receiver.PubKey[:])
	require.NoError(t, err)
	placeholder := mustPayToTaproot(
		t, txscript.ComputeTaprootKeyNoScript(internalKey),
	)
	tx := wire.NewMsgTx(customAnchorITestVersion)
	tx.AddTxIn(wire.NewTxIn(&outpoint, nil, nil))
	tx.AddTxOut(&wire.TxOut{Value: value, PkScript: placeholder})
	tx.TxIn[0].Sequence = customAnchorITestSequence
	if spend != nil {
		require.Equal(t, prevout.PkScript, spend.PkScript)
		tx.LockTime = spend.LockTime
		tx.TxIn[0].Sequence = spend.Sequence
	}
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	input := &packet.Inputs[0]
	input.WitnessUtxo = prevout
	if spend != nil {
		input.TaprootInternalKey = spend.InternalKey[1:]
		input.TaprootMerkleRoot = spend.TaprootMerkleRoot[:]
		leaf := &psbt.TaprootTapLeafScript{
			ControlBlock: spend.ControlBlock,
			Script:       spend.LeafScript,
			LeafVersion:  txscript.BaseLeafVersion,
		}
		input.TaprootLeafScript = []*psbt.TaprootTapLeafScript{leaf}
	}
	return swapKitSerializePSBT(t, packet), value
}

// swapKitSerializePSBT encodes a caller-owned Bitcoin anchor template.
func swapKitSerializePSBT(t testing.TB, packet *psbt.Packet) []byte {
	t.Helper()
	var buffer bytes.Buffer
	require.NoError(t, packet.Serialize(&buffer))
	return buffer.Bytes()
}
