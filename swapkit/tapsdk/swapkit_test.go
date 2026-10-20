package tapsdk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// TestSwapKitContractVectors freezes the Bitcoin contract vectors from Loop's
// shared-asset-kit HTLC tests, including the extra asset commitment sibling.
// V3 differs only in its CLTV refund opcode for these otherwise equal inputs.
func TestSwapKitContractVectors(t *testing.T) {
	params := swapKitTestParams(t)
	for i := range params.PaymentHash {
		params.PaymentHash[i] = byte(i)
	}
	root := swapKitTestAssetRoot()
	const successHex = "20c6047f9441ed7d6d3045406e95c07cd85c778e" +
		"4b8cef3ca7abac09b95c709ee5ad82012088a914e6babb9619d7a812" +
		"72711fc546a16b211dd939578851b2"
	const timeoutPrefix = "2079be667ef9dcbbac55a06295ce870b07029bfcdb2d" +
		"ce28d959f2815b16f81798ad029000"
	const aggregateHex = "023b46d262d2f610e9038b44beabdfe97ab5a0feb" +
		"89870acc2264edfb7f63ec2ec"
	const successControlHex = "c13b46d262d2f610e9038b44beabdfe97ab5a0feb" +
		"89" +
		"870acc2264edfb7f63ec2eccbf40123d0ca1191c0dba575c7a92ea255" +
		"f9e9fc81484e3929afeaa1a6f459d6a0a1a2a3a4a5a6a7a8a9aaab" +
		"acadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf"
	const timeoutControlHex = "c13b46d262d2f610e9038b44beabdfe97ab5a0feb" +
		"89" +
		"870acc2264edfb7f63ec2ec00114fbfd01b5de82dfa6f54f0f27af41" +
		"d5a66ed39ac1a87d614fa12808f9adfa0a1a2a3a4a5a6a7a8a9aaab" +
		"acadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf"
	const anchorHex = "512021b15bddf2d991b8791ec7c2105fa69be61008d78" +
		"b67d261451290d7a44dce5f"

	legacy, err := NewSwapKit(params)
	require.NoError(t, err)
	success, err := legacy.SuccessSpend(root)
	require.NoError(t, err)
	timeout, err := legacy.TimeoutSpend(root)
	require.NoError(t, err)
	require.Equal(t, successHex, hex.EncodeToString(success.LeafScript))
	require.Equal(t, timeoutPrefix+"b2",
		hex.EncodeToString(timeout.LeafScript))
	require.Equal(t, aggregateHex,
		legacy.AnchorPlan().InternalKey.PubKey.String())
	require.Equal(t, successControlHex,
		hex.EncodeToString(success.ControlBlock))
	require.Equal(t, timeoutControlHex,
		hex.EncodeToString(timeout.ControlBlock))
	require.Equal(t, anchorHex, hex.EncodeToString(success.PkScript))
	require.Equal(t, success.PkScript, timeout.PkScript)

	params.Policy = SwapHTLCStaticLoopInV3
	v3, err := NewSwapKit(params)
	require.NoError(t, err)
	v3Success, err := v3.SuccessSpend(root)
	require.NoError(t, err)
	v3Timeout, err := v3.TimeoutSpend(root)
	require.NoError(t, err)
	require.Equal(t, successHex, hex.EncodeToString(v3Success.LeafScript))
	require.Equal(t, timeoutPrefix+"b1",
		hex.EncodeToString(v3Timeout.LeafScript))
	require.NotEqual(t, success.PkScript, v3Success.PkScript)
	require.Equal(t, uint32(144), v3Timeout.LockTime)
	require.Equal(t, wire.MaxTxInSequenceNum-1, v3Timeout.Sequence)

	deposit, err := NewDepositKit(DepositKitParams{
		ClientKey: params.SenderKey, ServerKey: params.ReceiverKey,
		CSVExpiry: 144,
	})
	require.NoError(t, err)
	depositTimeout, err := deposit.TimeoutSpend(root)
	require.NoError(t, err)
	require.Equal(t, timeout.LeafScript, depositTimeout.LeafScript)
	require.Equal(t, legacy.AnchorPlan().InternalKey,
		deposit.AnchorPlan().InternalKey)
	require.Len(t, depositTimeout.ControlBlock, 65)
	require.Len(t, success.ControlBlock, 97)
}

// TestSwapKitScriptExecution verifies authorization and timelock boundaries in
// the Bitcoin VM, signing each altered transaction afresh so a failed timelock
// cannot be masked by an unrelated invalid signature.
func TestSwapKitScriptExecution(t *testing.T) {
	for _, policy := range []SwapHTLCPolicy{
		SwapHTLCLegacyCSV, SwapHTLCStaticLoopInV3,
	} {
		for _, success := range []bool{true, false} {
			name := swapKitTestName(policy, success, "")
			t.Run(name, func(t *testing.T) {
				testSwapKitScriptExecution(t, policy, success)
			})
		}
	}
}

func testSwapKitScriptExecution(t *testing.T, policy SwapHTLCPolicy,
	success bool) {

	t.Helper()
	params := swapKitTestParams(t)
	params.Policy = policy
	kit, err := NewSwapKit(params)
	require.NoError(t, err)
	type executionCase struct {
		name     string
		mutateTx func(*wire.MsgTx)
		mutate   func([][]byte)
		wrongKey bool
		valid    bool
	}
	tests := []executionCase{
		{name: "valid", valid: true},
		{name: "wrong signer", wrongKey: true},
		{
			name: "wrong asset commitment",
			mutate: func(w [][]byte) {
				w[len(w)-1][len(w[len(w)-1])-1] ^= 1
			},
		},
	}
	if success || policy == SwapHTLCLegacyCSV {
		tests = append(tests, []executionCase{
			{
				name: "early CSV",
				mutateTx: func(tx *wire.MsgTx) {
					tx.TxIn[0].Sequence--
				},
			},
			{
				name: "version one",
				mutateTx: func(tx *wire.MsgTx) {
					tx.Version = 1
				},
			},
			{
				name: "CSV disabled",
				mutateTx: func(tx *wire.MsgTx) {
					tx.TxIn[0].Sequence = 0x80000090
				},
			},
			{
				name: "time-based CSV",
				mutateTx: func(tx *wire.MsgTx) {
					tx.TxIn[0].Sequence = 0x00400090
				},
			},
		}...)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := swapKitTestAssetRoot()
			spend, scalar := swapKitTestSpend(t, kit, root, success)
			if test.wrongKey {
				scalar = 3
			}
			tx, prevOut, sig := swapKitSignTestTx(
				t, spend, scalar, test.mutateTx,
			)
			var witness [][]byte
			var err error
			if success {
				witness, err = kit.SuccessWitness(
					root, sig, [32]byte{42},
				)
			} else {
				witness, err = kit.TimeoutWitness(root, sig)
			}
			require.NoError(t, err)
			if test.mutate != nil {
				test.mutate(witness)
			}
			tx.TxIn[0].Witness = witness
			err = swapKitExecuteTestTx(tx, prevOut)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// TestSwapKitCLTVAndPreimage checks both on-chain enforcement and the kit's
// preimage boundary, including transaction locktime type and final sequences.
func TestSwapKitCLTVAndPreimage(t *testing.T) {
	params := swapKitTestParams(t)
	params.Policy = SwapHTLCStaticLoopInV3
	kit, err := NewSwapKit(params)
	require.NoError(t, err)
	root := swapKitTestAssetRoot()
	spend, err := kit.TimeoutSpend(root)
	require.NoError(t, err)
	for _, test := range []struct {
		name   string
		mutate func(*wire.MsgTx)
	}{
		{"early height", func(tx *wire.MsgTx) { tx.LockTime-- }},
		{"time lock", func(tx *wire.MsgTx) {
			tx.LockTime = 500_000_000
		}},
		{"final sequence", func(tx *wire.MsgTx) {
			tx.TxIn[0].Sequence = wire.MaxTxInSequenceNum
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, prevOut, sig := swapKitSignTestTx(
				t, spend, 1, test.mutate,
			)
			tx.TxIn[0].Witness, err = kit.TimeoutWitness(root, sig)
			require.NoError(t, err)
			require.Error(t, swapKitExecuteTestTx(tx, prevOut))
		})
	}

	spend, err = kit.SuccessSpend(root)
	require.NoError(t, err)
	for _, preimage := range [][]byte{bytes.Repeat([]byte{7}, 32), {42}} {
		tx, prevOut, sig := swapKitSignTestTx(t, spend, 2, nil)
		witness, err := kit.SuccessWitness(root, sig, [32]byte{42})
		require.NoError(t, err)
		witness[0] = preimage
		tx.TxIn[0].Witness = witness
		require.Error(t, swapKitExecuteTestTx(tx, prevOut))
		_, err = kit.SuccessWitness(root, sig, [32]byte{7})
		require.ErrorContains(t, err, "preimage does not match")
	}
}

// TestDepositKitScriptExecution verifies that the client alone can refund only
// with the correct version and block-based CSV sequence.
func TestDepositKitScriptExecution(t *testing.T) {
	params := swapKitTestParams(t)
	kit, err := NewDepositKit(DepositKitParams{
		ClientKey: params.SenderKey, ServerKey: params.ReceiverKey,
		CSVExpiry: 144,
	})
	require.NoError(t, err)
	root := swapKitTestAssetRoot()
	spend, err := kit.TimeoutSpend(root)
	require.NoError(t, err)
	for _, test := range []struct {
		name   string
		mutate func(*wire.MsgTx)
		key    byte
		valid  bool
	}{
		{name: "mature", key: 1, valid: true},
		{name: "server cannot refund", key: 2},
		{name: "early", key: 1, mutate: func(tx *wire.MsgTx) {
			tx.TxIn[0].Sequence--
		}},
		{name: "version one", key: 1, mutate: func(tx *wire.MsgTx) {
			tx.Version = 1
		}},
		{name: "CSV disabled", key: 1, mutate: func(tx *wire.MsgTx) {
			tx.TxIn[0].Sequence |= wire.SequenceLockTimeDisabled
		}},
		{name: "time-based CSV", key: 1, mutate: func(tx *wire.MsgTx) {
			tx.TxIn[0].Sequence |= wire.SequenceLockTimeIsSeconds
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, prevOut, sig := swapKitSignTestTx(
				t, spend, test.key, test.mutate,
			)
			tx.TxIn[0].Witness, err = kit.TimeoutWitness(root, sig)
			require.NoError(t, err)
			err = swapKitExecuteTestTx(tx, prevOut)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// TestSwapKitValidation checks malformed parameters before construction and
// ensures callers cannot mutate a funded kit through returned slice aliases.
func TestSwapKitValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*SwapKitParams)
	}{
		{"no policy", func(p *SwapKitParams) {
			p.Policy = SwapHTLCUnspecified
		}},
		{"unknown policy", func(p *SwapKitParams) { p.Policy = 99 }},
		{"zero hash", func(p *SwapKitParams) {
			p.PaymentHash = sdk.Hash{}
		}},
		{"bad key", func(p *SwapKitParams) {
			p.SenderKey = sdk.PubKey{}
		}},
		{"same key", func(p *SwapKitParams) {
			p.ReceiverKey = p.SenderKey
		}},
		{"opposite key", func(p *SwapKitParams) {
			p.ReceiverKey = p.SenderKey
			p.ReceiverKey[0] ^= 1
		}},
		{"same internal key", func(p *SwapKitParams) {
			p.ReceiverInternalKey = p.SenderInternalKey
		}},
		{"zero expiry", func(p *SwapKitParams) { p.Expiry = 0 }},
		{"large CSV", func(p *SwapKitParams) { p.Expiry = 65536 }},
		{"timestamp CLTV", func(p *SwapKitParams) {
			p.Policy, p.Expiry = SwapHTLCStaticLoopInV3, 500_000_000
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := swapKitTestParams(t)
			test.mutate(&params)
			_, err := NewSwapKit(params)
			require.Error(t, err)
		})
	}

	kit, err := NewSwapKit(swapKitTestParams(t))
	require.NoError(t, err)
	plan := kit.AnchorPlan()
	expected := bytes.Clone(plan.Tapscript.TapLeaves[0].Script)
	plan.Tapscript.TapLeaves[0].Script[0] ^= 1
	require.Equal(t, expected,
		kit.AnchorPlan().Tapscript.TapLeaves[0].Script)
	_, err = kit.SuccessSpend(sdk.Hash{})
	require.ErrorContains(t, err, "asset commitment root")
	_, err = kit.TimeoutWitness(swapKitTestAssetRoot(), nil)
	require.ErrorContains(t, err, "signature must be")

	context := []byte("unique-signing-round")
	signing := kit.CooperativeSigningPlan(2, context)
	require.Equal(t, uint32(2), signing.InputIndex)
	require.NoError(t, validateMuSig2Aggregate(
		kit.AnchorPlan().InternalKey.PubKey.XOnly(),
		signing.MuSig2.Participants,
	))
	context[0] ^= 1
	require.NotEqual(t, context, signing.MuSig2.SessionContext)
	signing.MuSig2.Participants[0] = sdk.PubKey{}
	require.NotEqual(t, signing.MuSig2.Participants,
		kit.CooperativeSigningPlan(2, nil).MuSig2.Participants)
}

// TestSwapKitOutput checks OP_TRUE output descriptors, native amount bounds,
// and defensive copies before passing them to the custom-anchor builder.
func TestSwapKitOutput(t *testing.T) {
	kit, err := NewSwapKit(swapKitTestParams(t))
	require.NoError(t, err)
	params := SwapOutputParams{
		ID:       "deposit",
		AssetRef: sdk.AssetRefFromAssetID(sdk.AssetID{1}),
		Amount:   1_000, AnchorValueSat: 330,
		AssetInternalKey: sdk.KeyDescriptor{RawKeyBytes: testKey(t, 3)},
		ProofDelivery: sdk.CustomAssetProofDelivery{
			OpaqueMetadata: []byte{1},
		},
	}
	output, err := kit.Output(params)
	require.NoError(t, err)
	require.Equal(t, sdk.CustomAssetScriptOPTrue, output.Script.Mode)
	require.Equal(t, sdk.CustomAssetTimelocks{}, output.Timelocks)
	require.Equal(t, kit.AnchorPlan(), output.Anchor)
	params.ProofDelivery.OpaqueMetadata[0] = 2
	require.Equal(t, []byte{1}, output.ProofDelivery.OpaqueMetadata)
	params.Amount = math.MaxInt64 + 1
	_, err = kit.Output(params)
	require.ErrorContains(t, err, "amount exceeds maximum")
	params.Amount = 1
	params.AnchorValueSat = math.MaxInt64 + 1
	_, err = kit.Output(params)
	require.ErrorContains(t, err, "amount exceeds maximum")
	params.AnchorValueSat = 330
	params.AssetInternalKey = sdk.KeyDescriptor{}
	_, err = kit.Output(params)
	require.ErrorContains(t, err, "OP_TRUE internal key")
}

// TestSwapKitBuilderCommitments binds the kit's Bitcoin control blocks to the
// custom-anchor builder's independently assembled asset and policy roots.
func TestSwapKitBuilderCommitments(t *testing.T) {
	params := swapKitTestParams(t)
	legacy, err := NewSwapKit(params)
	require.NoError(t, err)
	params.Policy = SwapHTLCStaticLoopInV3
	v3, err := NewSwapKit(params)
	require.NoError(t, err)
	deposit, err := NewDepositKit(DepositKitParams{
		ClientKey: params.SenderKey, ServerKey: params.ReceiverKey,
		CSVExpiry: 144,
	})
	require.NoError(t, err)
	for name, kit := range map[string]swapKitOutput{
		"legacy": legacy, "v3": v3, "deposit": deposit,
	} {
		t.Run(name, func(t *testing.T) {
			first := testSwapKitBuilderCommitment(t, kit, 10)
			second := testSwapKitBuilderCommitment(t, kit, 11)
			require.NotEqual(t, first, second)
		})
	}
}

type swapKitOutput interface {
	Output(SwapOutputParams) (sdk.CustomAssetOutput, error)
	TimeoutSpend(sdk.Hash) (*SwapAnchorSpendInfo, error)
}

func testSwapKitBuilderCommitment(t *testing.T, kit swapKitOutput,
	scalar byte) sdk.PubKey {

	t.Helper()
	fixture := newCustomAnchorBuilderFixture(t)
	original := fixture.request.Outputs[0]
	output, err := kit.Output(SwapOutputParams{
		ID: original.ID, AssetRef: original.AssetRef,
		Amount:            original.Amount,
		AnchorOutputIndex: original.AnchorOutputIndex,
		AnchorValueSat:    original.AnchorValueSat,
		AssetInternalKey: sdk.KeyDescriptor{
			RawKeyBytes: testKey(t, scalar),
		},
	})
	require.NoError(t, err)
	fixture.request.Outputs[0] = output
	wallet := sdk.NewWallet(fixture.client, sdk.NetworkRegtest)
	plan, err := wallet.NewCustomAnchorTxBuilder().
		Build(t.Context(), fixture.request)
	require.NoError(t, err)
	planned := plan.Outputs()[0]
	require.NotNil(t, planned.OPTrueSpend)
	require.NoError(t, planned.OPTrueSpend.Validate(planned.ScriptKey))
	require.Equal(t, []byte{txscript.OP_TRUE},
		planned.OPTrueSpend.LeafScript)

	preview, err := plan.PreviewOutputCommitments()
	require.NoError(t, err)
	require.Len(t, preview, 1)
	spend, err := kit.TimeoutSpend(preview[0].TaprootAssetRoot)
	require.NoError(t, err)
	require.Equal(t, preview[0].TaprootMerkleRoot, spend.TaprootMerkleRoot)
	control, err := txscript.ParseControlBlock(spend.ControlBlock)
	require.NoError(t, err)
	require.NoError(t, txscript.VerifyTaprootLeafCommitment(
		control, spend.PkScript[2:], spend.LeafScript,
	))
	return planned.ScriptKey
}

// TestSwapKitSignatureEncoding rejects malformed Schnorr encodings and the
// forbidden explicit SIGHASH_DEFAULT byte; a valid explicit hash mode spends.
func TestSwapKitSignatureEncoding(t *testing.T) {
	kit, err := NewSwapKit(swapKitTestParams(t))
	require.NoError(t, err)
	root := swapKitTestAssetRoot()
	spend, err := kit.TimeoutSpend(root)
	require.NoError(t, err)
	tx, prevOut, signature := swapKitSignTestTx(t, spend, 1, nil)
	for _, invalid := range [][]byte{
		nil, signature[:63], append(bytes.Clone(signature), 0),
		append(bytes.Clone(signature), 4),
		append(bytes.Clone(signature), 0x80),
		bytes.Repeat([]byte{0xff}, 64),
	} {
		_, err := kit.TimeoutWitness(root, invalid)
		require.Error(t, err)
	}
	fetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	keyBytes := make([]byte, 32)
	keyBytes[31] = 1
	key, _ := btcec.PrivKeyFromBytes(keyBytes)
	signature, err = txscript.RawTxInTapscriptSignature(
		tx, txscript.NewTxSigHashes(tx, fetcher), 0, prevOut.Value,
		prevOut.PkScript, txscript.NewBaseTapLeaf(spend.LeafScript),
		txscript.SigHashAll, key,
	)
	require.NoError(t, err)
	require.Len(t, signature, 65)
	tx.TxIn[0].Witness, err = kit.TimeoutWitness(root, signature)
	require.NoError(t, err)
	require.NoError(t, swapKitExecuteTestTx(tx, prevOut))
	signature[0] ^= 1
	require.NotEqual(t, signature, tx.TxIn[0].Witness[0])
}

func swapKitTestParams(t *testing.T) SwapKitParams {
	t.Helper()
	sender := testKey(t, 1)
	receiver := testKey(t, 2)
	preimage := [32]byte{42}
	return SwapKitParams{
		Policy:    SwapHTLCLegacyCSV,
		SenderKey: sender, ReceiverKey: receiver,
		SenderInternalKey: sender, ReceiverInternalKey: receiver,
		PaymentHash: sdk.Hash(sha256.Sum256(preimage[:])),
		Expiry:      144,
	}
}

func swapKitTestAssetRoot() sdk.Hash {
	var root sdk.Hash
	for i := range root {
		root[i] = byte(0xa0 + i)
	}
	return root
}

func swapKitTestName(policy SwapHTLCPolicy, success bool, name string) string {
	prefix := "legacy/"
	if policy == SwapHTLCStaticLoopInV3 {
		prefix = "v3/"
	}
	if success {
		return prefix + "success/" + name
	}
	return prefix + "timeout/" + name
}

func swapKitTestSpend(t *testing.T, kit *SwapKit, root sdk.Hash,
	success bool) (*SwapAnchorSpendInfo, byte) {

	t.Helper()
	if success {
		spend, err := kit.SuccessSpend(root)
		require.NoError(t, err)
		return spend, 2
	}
	spend, err := kit.TimeoutSpend(root)
	require.NoError(t, err)
	return spend, 1
}

func swapKitSignTestTx(t *testing.T, spend *SwapAnchorSpendInfo,
	scalar byte, mutate func(*wire.MsgTx)) (
	*wire.MsgTx, *wire.TxOut, []byte) {

	t.Helper()
	tx := wire.NewMsgTx(2)
	tx.LockTime = spend.LockTime
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 1}, nil, nil))
	tx.TxIn[0].Sequence = spend.Sequence
	tx.AddTxOut(&wire.TxOut{Value: 900, PkScript: spend.PkScript})
	prevOut := &wire.TxOut{Value: 1_000, PkScript: spend.PkScript}
	if mutate != nil {
		mutate(tx)
	}
	fetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	keyBytes := make([]byte, 32)
	keyBytes[31] = scalar
	key, _ := btcec.PrivKeyFromBytes(keyBytes)
	sig, err := txscript.RawTxInTapscriptSignature(
		tx, txscript.NewTxSigHashes(tx, fetcher), 0, prevOut.Value,
		prevOut.PkScript, txscript.NewBaseTapLeaf(spend.LeafScript),
		txscript.SigHashDefault, key,
	)
	require.NoError(t, err)
	return tx, prevOut, sig
}

func swapKitExecuteTestTx(tx *wire.MsgTx, prevOut *wire.TxOut) error {
	fetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	vm, err := txscript.NewEngine(
		prevOut.PkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		txscript.NewTxSigHashes(tx, fetcher), prevOut.Value, fetcher,
	)
	if err != nil {
		return err
	}
	return vm.Execute()
}
