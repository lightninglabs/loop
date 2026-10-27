package swapkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestSwapKitIndependentRoles executes both HTLC versions in both on-chain
// directions with separate cooperative and unilateral signing keys.
func TestSwapKitIndependentRoles(t *testing.T) {
	for _, policy := range []SwapHTLCPolicy{
		SwapHTLCLegacyCSV, SwapHTLCCLTV,
	} {
		for _, reverse := range []bool{false, true} {
			name := swapKitTestName(policy, false, "loop in")
			if reverse {
				name = swapKitTestName(policy, false, "loop out")
			}
			t.Run(name, func(t *testing.T) {
				params := swapKitTestParams(t)
				params.Policy = policy
				params.SenderKey, params.ReceiverKey = testKey(t, 3), testKey(t, 4)
				sender, receiver := byte(3), byte(4)
				senderInternal, receiverInternal := byte(1), byte(2)
				if reverse {
					params.SenderKey, params.ReceiverKey = params.ReceiverKey, params.SenderKey
					params.SenderInternalKey, params.ReceiverInternalKey =
						params.ReceiverInternalKey, params.SenderInternalKey
					sender, receiver = receiver, sender
					senderInternal, receiverInternal = receiverInternal, senderInternal
				}
				kit, err := NewSwapKit(params)
				require.NoError(t, err)
				root := swapKitTestAssetRoot()
				for _, success := range []bool{false, true} {
					spend, err := kit.TimeoutSpend(root)
					key, wrong := sender, senderInternal
					if success {
						spend, err = kit.SuccessSpend(root)
						key, wrong = receiver, receiverInternal
					}
					require.NoError(t, err)
					for _, scalar := range []byte{key, wrong} {
						tx, previous, signature := swapKitSignTestTx(t, spend, scalar, nil)
						if success {
							tx.TxIn[0].Witness, err = kit.SuccessWitness(root,
								signature, [32]byte{42})
						} else {
							tx.TxIn[0].Witness, err = kit.TimeoutWitness(root, signature)
						}
						require.NoError(t, err)
						err = swapKitExecuteTestTx(tx, previous)
						if scalar == key {
							require.NoError(t, err)
						} else {
							require.Error(t, err)
						}
					}
				}
			})
		}
	}
}

func TestSwapKitKeyRoleValidation(t *testing.T) {
	for _, opposite := range []bool{false, true} {
		for _, internal := range []bool{false, true} {
			params := swapKitTestParams(t)
			params.SenderKey, params.ReceiverKey = testKey(t, 3), testKey(t, 4)
			alias := params.SenderInternalKey
			if internal {
				alias = params.SenderKey
			}
			if opposite {
				alias[0] ^= 1
			}
			params.ReceiverKey = alias
			_, err := NewSwapKit(params)
			require.Error(t, err)
			params.ReceiverKey = testKey(t, 4)
			params.ReceiverInternalKey = alias
			_, err = NewSwapKit(params)
			require.Error(t, err)
		}
	}
	params := swapKitTestParams(t)
	params.SenderKey, params.ReceiverKey = PubKey{}, PubKey{}
	params.SenderScriptKey, params.ReceiverScriptKey = testKey(t, 3), testKey(t, 4)
	kit, err := NewSwapKit(params)
	require.NoError(t, err)
	require.Equal(t, params.SenderScriptKey, kit.Params().SenderKey)
	require.Equal(t, params.ReceiverScriptKey, kit.Params().ReceiverKey)
	params.SenderKey = testKey(t, 1)
	_, err = NewSwapKit(params)
	require.ErrorContains(t, err, "conflicting sender")
	params.SenderKey = PubKey{}
	params.ReceiverKey = testKey(t, 2)
	_, err = NewSwapKit(params)
	require.ErrorContains(t, err, "conflicting receiver")
}

// TestDepositKitIndependentRoles retains the existing CSV deposit while
// separating its funder's cooperative key from its refund signer.
func TestDepositKitIndependentRoles(t *testing.T) {
	params := DepositKitParams{
		SenderInternalKey: testKey(t, 1), ReceiverInternalKey: testKey(t, 2),
		SenderScriptKey: testKey(t, 3), ReceiverScriptKey: testKey(t, 4),
		CSVExpiry: 144,
	}
	kit, err := NewDepositKit(params)
	require.NoError(t, err)
	spend, err := kit.TimeoutSpend(swapKitTestAssetRoot())
	require.NoError(t, err)
	require.Equal(t, [32]byte(params.SenderScriptKey[1:]), spend.RequiredSigner)
	for _, scalar := range []byte{3, 1, 2, 4} {
		tx, previous, signature := swapKitSignTestTx(t, spend, scalar, nil)
		tx.TxIn[0].Witness, err = kit.TimeoutWitness(swapKitTestAssetRoot(), signature)
		require.NoError(t, err)
		err = swapKitExecuteTestTx(tx, previous)
		if scalar == 3 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
	invalid := params
	invalid.ClientKey = testKey(t, 1)
	_, err = NewDepositKit(invalid)
	require.ErrorContains(t, err, "cannot mix")
	invalid = params
	invalid.ReceiverScriptKey = PubKey{}
	_, err = NewDepositKit(invalid)
	require.Error(t, err)
	invalid = params
	invalid.ReceiverScriptKey = params.SenderInternalKey
	_, err = NewDepositKit(invalid)
	require.Error(t, err)
}

// TestReservationKit executes the server-funded absolute refund independently
// of static deposits' CSV clock and rejects early or wrong-key refunds.
func TestReservationKit(t *testing.T) {
	params := ReservationKitParams{
		ServerKey: testKey(t, 2), ClientKey: testKey(t, 1), RefundHeight: 1_000_000,
	}
	legacy, err := NewReservationKit(params)
	require.NoError(t, err)
	require.Equal(t, "20c6047f9441ed7d6d3045406e95c07cd85c778e"+
		"4b8cef3ca7abac09b95c709ee5ad0340420fb1",
		hex.EncodeToString(legacy.TimeoutScript()))
	spend, err := legacy.TimeoutSpend(swapKitTestAssetRoot())
	require.NoError(t, err)
	require.Len(t, spend.ControlBlock, 65)
	require.Equal(t, wire.MaxTxInSequenceNum-2, spend.Sequence)
	require.Equal(t, params.RefundHeight, spend.LockTime)
	require.Equal(t, [32]byte(params.ServerKey[1:]), spend.RequiredSigner)
	for _, test := range []struct {
		name   string
		mutate func(*wire.MsgTx)
		key    byte
		valid  bool
	}{
		{name: "server refund", key: 2, valid: true},
		{name: "client cannot refund", key: 1},
		{name: "early", key: 2, mutate: func(tx *wire.MsgTx) { tx.LockTime-- }},
		{name: "later", key: 2, valid: true, mutate: func(tx *wire.MsgTx) { tx.LockTime++ }},
		{name: "final sequence", key: 2, mutate: func(tx *wire.MsgTx) {
			tx.TxIn[0].Sequence = wire.MaxTxInSequenceNum
		}},
		{name: "timestamp", key: 2, mutate: func(tx *wire.MsgTx) {
			tx.LockTime = txscript.LockTimeThreshold
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, previous, signature := swapKitSignTestTx(t, spend, test.key, test.mutate)
			tx.TxIn[0].Witness, err = legacy.TimeoutWitness(swapKitTestAssetRoot(), signature)
			require.NoError(t, err)
			err = swapKitExecuteTestTx(tx, previous)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	explicit, err := NewReservationKit(ReservationKitParams{
		SenderInternalKey: params.ServerKey, ReceiverInternalKey: params.ClientKey,
		SenderScriptKey: testKey(t, 4), ReceiverScriptKey: testKey(t, 3),
		RefundHeight: params.RefundHeight,
	})
	require.NoError(t, err)
	require.Equal(t, legacy.AnchorPlan().InternalKey, explicit.AnchorPlan().InternalKey)
	spend, err = explicit.TimeoutSpend(swapKitTestAssetRoot())
	require.NoError(t, err)
	for _, scalar := range []byte{4, 2} {
		tx, previous, signature := swapKitSignTestTx(t, spend, scalar, nil)
		tx.TxIn[0].Witness, err = explicit.TimeoutWitness(swapKitTestAssetRoot(), signature)
		require.NoError(t, err)
		err = swapKitExecuteTestTx(tx, previous)
		if scalar == 4 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
}

func TestReservationKitValidation(t *testing.T) {
	params := ReservationKitParams{ServerKey: testKey(t, 2), ClientKey: testKey(t, 1)}
	for _, height := range []uint32{0, txscript.LockTimeThreshold, ^uint32(0)} {
		params.RefundHeight = height
		_, err := NewReservationKit(params)
		require.Error(t, err)
	}
	for _, height := range []uint32{1, txscript.LockTimeThreshold - 1} {
		params.RefundHeight = height
		_, err := NewReservationKit(params)
		require.NoError(t, err)
	}
	params.RefundHeight = 100
	params.SenderScriptKey = params.ServerKey
	_, err := NewReservationKit(params)
	require.ErrorContains(t, err, "cannot mix")
	params.SenderScriptKey = PubKey{}
	params.ClientKey = params.ServerKey
	_, err = NewReservationKit(params)
	require.Error(t, err)
}

func TestSwapKitSnapshots(t *testing.T) {
	params := swapKitTestParams(t)
	kit, err := NewSwapKit(params)
	require.NoError(t, err)
	plan := kit.AnchorPlan()
	plan.Leaves[0][0] ^= 1
	participants := kit.CooperativeParticipants()
	participants[0][0] ^= 1
	copyParams := kit.Params()
	copyParams.PaymentHash = sha256.Sum256([]byte{1})
	require.Equal(t, params, kit.Params())
	require.NotEqual(t, participants, kit.CooperativeParticipants())
	require.NotEqual(t, plan.Leaves, kit.AnchorPlan().Leaves)
	spend, err := kit.SuccessSpend(swapKitTestAssetRoot())
	require.NoError(t, err)
	spend.LeafScript[0] ^= 1
	spend.PkScript[0] ^= 1
	spend.ControlBlock[0] ^= 1
	fresh, err := kit.SuccessSpend(swapKitTestAssetRoot())
	require.NoError(t, err)
	require.False(t, bytes.Equal(spend.LeafScript, fresh.LeafScript))
	require.NotEqual(t, spend.PkScript, fresh.PkScript)
	require.NotEqual(t, spend.ControlBlock, fresh.ControlBlock)
}

func TestFlowRecordDecode(t *testing.T) {
	var offer Offer
	require.NoError(t, Decode([]byte(`{"Hash":[1],"Terms":{"Version":1}}`), &offer))
	require.Equal(t, uint32(1), offer.Terms.Version)
	require.Error(t, Decode([]byte(`{"Terms":{"Unknown":1}}`), &offer))
	require.Error(t, Decode([]byte(`{} {}`), &offer))
}
