package htlc

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

func cltvParams(t *testing.T) Params {
	t.Helper()

	params := vectorParams(t)
	params.CsvExpiry = 0
	params.RefundHeight = 1_000_000
	return params
}

// TestCLTVContract freezes the new leaf and compares both leaves to the
// Bitcoin V3 scripts used by static Loop In. The asset key stays unchanged.
func TestCLTVContract(t *testing.T) {
	params := cltvParams(t)
	kit, err := NewSwapKit(AssetHTLCCLTVV1, params)
	require.NoError(t, err)
	params.RefundHeight++
	require.Equal(t, uint32(1_000_000), kit.RefundHeight())
	require.Zero(t, kit.CsvExpiry())
	require.Equal(t, Policy(2), kit.Policy())

	success, err := kit.GetSuccessScript()
	require.NoError(t, err)
	require.Equal(t, legacySuccessScript, hex.EncodeToString(success))
	expected, err := swap.GenSuccessPathScript(
		kit.ReceiverScriptKey(), kit.SwapHash(),
	)
	require.NoError(t, err)
	require.Equal(t, expected, success)
	timeout, err := kit.GetTimeoutScript()
	require.NoError(t, err)
	require.Equal(t, "2079be667ef9dcbbac55a06295ce870b07029bfcdb2d"+
		"ce28d959f2815b16f81798ad0340420fb1", hex.EncodeToString(timeout))
	expected, err = swap.GenTimeoutPathScript(
		kit.SenderScriptKey(), int64(kit.RefundHeight()),
	)
	require.NoError(t, err)
	require.Equal(t, expected, timeout)

	packet, err := kit.CreateHtlcVpkt()
	require.NoError(t, err)
	require.Equal(t, legacyOpTrueScriptKey, hex.EncodeToString(
		packet.Outputs[1].ScriptKey.PubKey.SerializeCompressed(),
	))
	require.Equal(t, SpendConstraints{Sequence: SuccessSequence},
		kit.SuccessSpendConstraints())
	require.Equal(t, SpendConstraints{
		Sequence: wire.MaxTxInSequenceNum - 1, LockTime: 1_000_000,
	}, kit.TimeoutSpendConstraints())
	legacy := newVectorKit(t, AssetHTLCV0)
	require.Zero(t, legacy.RefundHeight())
	require.Equal(t, SpendConstraints{Sequence: 144},
		legacy.TimeoutSpendConstraints())
	require.Equal(t, legacy.SuccessSpendConstraints(), kit.SuccessSpendConstraints())
}

// TestCLTVTerms rejects ambiguous or timestamp-based refund terms, while
// allowing the full positive block-height range independently of CSV's range.
func TestCLTVTerms(t *testing.T) {
	for _, height := range []uint32{0, txscript.LockTimeThreshold, ^uint32(0)} {
		params := cltvParams(t)
		params.RefundHeight = height
		_, err := NewSwapKit(AssetHTLCCLTVV1, params)
		require.Error(t, err)
	}
	for _, height := range []uint32{1, txscript.LockTimeThreshold - 1} {
		params := cltvParams(t)
		params.RefundHeight = height
		_, err := NewSwapKit(AssetHTLCCLTVV1, params)
		require.NoError(t, err)
	}
	params := cltvParams(t)
	params.CsvExpiry = 144
	_, err := NewSwapKit(AssetHTLCCLTVV1, params)
	require.ErrorContains(t, err, "cannot set CSV")
	_, err = NewSwapKit(AssetHTLCV0, params)
	require.ErrorContains(t, err, "cannot set refund height")
	for _, height := range []int64{-1, 0, txscript.LockTimeThreshold} {
		_, err := GenCLTVTimeoutPathScript(params.SenderScriptKey, height)
		require.Error(t, err)
	}
	_, err = GenCLTVTimeoutPathScript(nil, 1)
	require.Error(t, err)
}

// TestCLTVScriptEngine checks consensus behavior independently of the kit's
// signing guard. Bitcoin permits any non-final sequence for the CLTV leaf;
// the kit selects a canonical value that also disables relative locks.
func TestCLTVScriptEngine(t *testing.T) {
	params := cltvParams(t)
	preimage := lntypes.Preimage{1}
	params.SwapHash = preimage.Hash()
	kit, err := NewSwapKit(AssetHTLCCLTVV1, params)
	require.NoError(t, err)
	sender, _ := scalarKey(t, 1)
	receiver, _ := scalarKey(t, 2)
	for _, test := range []struct {
		name     string
		sequence uint32
		lockTime uint32
		valid    bool
	}{
		{"early", wire.MaxTxInSequenceNum - 1, 999_999, false},
		{"height", wire.MaxTxInSequenceNum - 1, 1_000_000, true},
		{"later", wire.MaxTxInSequenceNum - 1, 1_000_001, true},
		{"timestamp", wire.MaxTxInSequenceNum - 1, txscript.LockTimeThreshold, false},
		{"final", wire.MaxTxInSequenceNum, 1_000_000, false},
		{"other non-final", 0, 1_000_000, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertScriptSpendWithLockTime(
				t, kit, false, test.sequence, test.lockTime,
				sender, preimage, test.valid,
			)
		})
	}
	assertScriptSpendWithLockTime(t, kit, false, wire.MaxTxInSequenceNum-1,
		1_000_000, receiver, preimage, false)
	assertScriptSpend(t, kit, true, SuccessSequence, receiver, preimage, true)
	assertScriptSpend(t, kit, true, 0, receiver, preimage, false)
	assertScriptSpend(t, kit, true, SuccessSequence, sender, preimage, false)
	assertScriptSpend(t, kit, true, SuccessSequence, receiver,
		lntypes.Preimage{2}, false)
}

// TestCLTVWitnesses validates actual virtual transfers and Bitcoin witnesses
// in both swap directions, preserving a pre-existing fee signature and PSBT.
func TestCLTVWitnesses(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, path := range []string{claimPath, refundPath} {
			name := "loop in/" + path
			if reverse {
				name = "loop out/" + path
			}
			t.Run(name, func(t *testing.T) {
				sequence := wire.MaxTxInSequenceNum - 1
				if path == claimPath {
					sequence = SuccessSequence
				}
				f := newWitnessFixtureForPolicy(
					t, sequence, asset.V1, commitment.TapCommitmentV2,
					reverse, AssetHTLCCLTVV1,
				)
				require.NoError(t, f.kit.ValidateProof(f.proof))
				if path == refundPath {
					f.packet.UnsignedTx.LockTime = f.kit.RefundHeight()
				}
				checkFeeSignature := signTimelockFeeInput(t, f)
				var before bytes.Buffer
				require.NoError(t, f.packet.Serialize(&before))
				signer := &localSigner{
					privateKey: f.receiverKey, expectedInput: f.assetInIndex,
				}
				var (
					witness *SpendWitness
					err     error
				)
				if path == claimPath {
					witness, err = f.kit.CreatePreimageWitness(
						t.Context(), signer, f.proof, f.packet,
						keychain.KeyLocator{}, f.preimage, f.transfer,
					)
				} else {
					signer.privateKey = f.senderKey
					witness, err = f.kit.CreateTimeoutWitness(
						t.Context(), signer, f.proof, f.packet,
						keychain.KeyLocator{}, f.transfer,
					)
				}
				require.NoError(t, err)
				var after bytes.Buffer
				require.NoError(t, f.packet.Serialize(&after))
				require.Equal(t, before.Bytes(), after.Bytes())
				checkFeeSignature()
				f.executeWitness(t, witness)
			})
		}
	}
}

// TestCLTVRefundConstraints rejects invalid timelocks before asking a signer,
// including a non-final but BIP68-enabled sequence on the proof-selected input.
func TestCLTVRefundConstraints(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence uint32
		lockTime uint32
		valid    bool
	}{
		{"unset locktime", wire.MaxTxInSequenceNum - 1, 0, false},
		{"early height", wire.MaxTxInSequenceNum - 1, 999_999, false},
		{"timestamp", wire.MaxTxInSequenceNum - 1, txscript.LockTimeThreshold, false},
		{"final", wire.MaxTxInSequenceNum, 1_000_000, false},
		{"relative delay", 144, 1_000_000, false},
		{"exact height", wire.MaxTxInSequenceNum - 1, 1_000_000, true},
		{"later height", wire.MaxTxInSequenceNum - 1, 1_000_001, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newWitnessFixtureForPolicy(
				t, test.sequence, asset.V1, commitment.TapCommitmentV2,
				false, AssetHTLCCLTVV1,
			)
			f.packet.UnsignedTx.LockTime = test.lockTime
			signer := &localSigner{
				privateKey: f.senderKey, expectedInput: f.assetInIndex,
			}
			witness, err := f.kit.CreateTimeoutWitness(
				t.Context(), signer, f.proof, f.packet,
				keychain.KeyLocator{}, f.transfer,
			)
			if !test.valid {
				require.Error(t, err)
				require.Zero(t, signer.calls)
				return
			}
			require.NoError(t, err)
			f.executeWitness(t, witness)
		})
	}
}

// TestCLTVProofBinding accepts a complete genesis-rooted history through tapd's
// verifier, and rejects reuse under a different policy or absolute height.
func TestCLTVProofBinding(t *testing.T) {
	f := newWitnessFixtureForPolicy(
		t, SuccessSequence, asset.V1, commitment.TapCommitmentV2,
		false, AssetHTLCCLTVV1,
	)
	issuance := htlcIssuanceProof(t, f)
	outpoint := issuance.OutPoint()
	accepted, err := f.kit.VerifyProofFile(
		t.Context(), tapdVerifier{}, encodeHtlcProofFile(t, *issuance),
		&outpoint,
	)
	require.NoError(t, err)
	require.Equal(t, outpoint, accepted.OutPoint())
	params := Params{
		SenderInternalPubKey:   f.kit.SenderInternalPubKey(),
		ReceiverInternalPubKey: f.kit.ReceiverInternalPubKey(),
		SenderScriptKey:        f.kit.SenderScriptKey(),
		ReceiverScriptKey:      f.kit.ReceiverScriptKey(),
		AssetID:                f.kit.AssetID(),
		Amount:                 f.kit.Amount(),
		SwapHash:               f.kit.SwapHash(),
		RefundHeight:           f.kit.RefundHeight() + 1,
		AddressParams:          &f.kit.addressParams,
	}
	other, err := NewSwapKit(AssetHTLCCLTVV1, params)
	require.NoError(t, err)
	require.Error(t, other.ValidateProof(accepted))
	params.RefundHeight = 0
	params.CsvExpiry = 144
	legacy, err := NewSwapKit(AssetHTLCV0, params)
	require.NoError(t, err)
	require.Error(t, legacy.ValidateProof(accepted))
}
