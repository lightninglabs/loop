package loopin

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

func TestLndTxOutChecker(t *testing.T) {
	fundingTx := wire.NewMsgTx(2)
	fundingTx.AddTxOut(wire.NewTxOut(1000, []byte{0x01}))
	fundingTx.AddTxOut(wire.NewTxOut(2000, []byte{0x02}))

	outpoint := wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: 1,
	}

	t.Run("returns live tx outputs", func(t *testing.T) {
		otherOutpoint := wire.OutPoint{
			Hash:  fundingTx.TxHash(),
			Index: 0,
		}
		client := &mockTxListLightningClient{
			txs: []lndclient.Transaction{{
				Tx: fundingTx,
			}},
		}

		checker := NewLndTxOutChecker(client)
		txOuts, err := checker.GetTxOuts(
			t.Context(), []wire.OutPoint{outpoint, otherOutpoint},
		)
		require.NoError(t, err)
		require.Equal(t, fundingTx.TxOut[outpoint.Index], txOuts[outpoint])
		require.Equal(
			t, fundingTx.TxOut[otherOutpoint.Index],
			txOuts[otherOutpoint],
		)
		require.Equal(t, []txListCall{{
			startHeight: 0,
			endHeight:   -1,
		}}, client.calls)
	})

	t.Run("returns nil for known spend", func(t *testing.T) {
		client := &mockTxListLightningClient{
			txs: []lndclient.Transaction{{
				Tx: fundingTx,
			}, {
				PreviousOutpoints: []*lnrpc.PreviousOutPoint{{
					Outpoint: outpoint.String(),
				}},
			}},
		}

		checker := NewLndTxOutChecker(client)
		txOuts, err := checker.GetTxOuts(
			t.Context(), []wire.OutPoint{outpoint},
		)
		require.NoError(t, err)
		require.Nil(t, txOuts[outpoint])
		require.Equal(t, []txListCall{{
			startHeight: 0,
			endHeight:   -1,
		}}, client.calls)
	})

	t.Run("returns error", func(t *testing.T) {
		expectedErr := errors.New("list transactions failed")
		client := &mockTxListLightningClient{
			err: expectedErr,
		}

		checker := NewLndTxOutChecker(client)
		txOuts, err := checker.GetTxOuts(
			t.Context(), []wire.OutPoint{outpoint},
		)
		require.ErrorIs(t, err, expectedErr)
		require.Nil(t, txOuts)
	})
}

type txListCall struct {
	startHeight int32
	endHeight   int32
}

type mockTxListLightningClient struct {
	lndclient.LightningClient

	txs   []lndclient.Transaction
	err   error
	calls []txListCall
}

func (m *mockTxListLightningClient) ListTransactions(_ context.Context,
	startHeight, endHeight int32, _ ...lndclient.ListTransactionsOption) (
	[]lndclient.Transaction, error) {

	m.calls = append(m.calls, txListCall{
		startHeight: startHeight,
		endHeight:   endHeight,
	})

	return m.txs, m.err
}
