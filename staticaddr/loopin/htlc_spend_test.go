package loopin

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/stretchr/testify/require"
)

// TestSpendsExactly tests that only a tx that spends exactly the given
// outpoints, in any order, spends them.
func TestSpendsExactly(t *testing.T) {
	op1 := wire.OutPoint{Hash: chainhash.Hash{1}}
	op2 := wire.OutPoint{Hash: chainhash.Hash{2}, Index: 1}
	op3 := wire.OutPoint{Hash: chainhash.Hash{3}}

	txSpending := func(outpoints ...wire.OutPoint) *wire.MsgTx {
		tx := wire.NewMsgTx(2)
		for _, outpoint := range outpoints {
			tx.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
		}

		return tx
	}

	outpoints := []wire.OutPoint{op1, op2}

	require.True(t, spendsExactly(txSpending(op1, op2), outpoints))
	require.True(t, spendsExactly(txSpending(op2, op1), outpoints))

	require.False(t, spendsExactly(nil, outpoints))
	require.False(t, spendsExactly(txSpending(), nil))
	require.False(t, spendsExactly(txSpending(op1), outpoints))
	require.False(t, spendsExactly(txSpending(op1, op2, op3), outpoints))
	require.False(t, spendsExactly(txSpending(op1, op3), outpoints))
	require.False(t, spendsExactly(txSpending(op1, op1), outpoints))
	require.False(t, spendsExactly(
		txSpending(op1, op2), []wire.OutPoint{op1, op1},
	))
}

// TestSignedOutpoints tests that a swap's signed outpoints are its outpoint
// snapshot, or its deposits if it has no snapshot.
func TestSignedOutpoints(t *testing.T) {
	op1 := wire.OutPoint{Hash: chainhash.Hash{1}}
	op2 := wire.OutPoint{Hash: chainhash.Hash{2}, Index: 1}

	loopIn := &StaticAddressLoopIn{
		DepositOutpoints: []string{op1.String(), op2.String()},
		Deposits:         []*deposit.Deposit{{OutPoint: op1}},
	}
	outpoints, err := loopIn.signedOutpoints()
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{op1, op2}, outpoints)

	loopIn.DepositOutpoints = nil
	outpoints, err = loopIn.signedOutpoints()
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{op1}, outpoints)

	loopIn.Deposits = []*deposit.Deposit{nil}
	_, err = loopIn.signedOutpoints()
	require.Error(t, err)

	loopIn.DepositOutpoints = []string{"not an outpoint"}
	_, err = loopIn.signedOutpoints()
	require.Error(t, err)
}
