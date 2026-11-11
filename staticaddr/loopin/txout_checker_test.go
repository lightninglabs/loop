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

// TestLndTxOutCheckerSpentByTxPayingTo checks that only a wallet transaction
// that spends one of the outpoints and pays to the pkScript counts.
func TestLndTxOutCheckerSpentByTxPayingTo(t *testing.T) {
	htlcPkScript := []byte{0x51, 0x20, 0x01}
	changePkScript := []byte{0x51, 0x20, 0x02}
	deposit := wire.OutPoint{Hash: [32]byte{1}, Index: 0}
	secondDeposit := wire.OutPoint{Hash: [32]byte{3}, Index: 1}
	otherOutpoint := wire.OutPoint{Hash: [32]byte{2}, Index: 0}

	// The lookup must list transactions from the given start height and
	// include unconfirmed ones.
	const startHeight int32 = 500

	spendingTx := func(prev wire.OutPoint, pkScript []byte) *wire.MsgTx {
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(wire.NewTxIn(&prev, nil, nil))
		tx.AddTxOut(wire.NewTxOut(1000, pkScript))

		return tx
	}

	// htlcTxWithChange spends a non-deposit outpoint and the second
	// deposit, and has a change output before the htlc output.
	htlcTxWithChange := wire.NewMsgTx(2)
	htlcTxWithChange.AddTxIn(wire.NewTxIn(&otherOutpoint, nil, nil))
	htlcTxWithChange.AddTxIn(wire.NewTxIn(&secondDeposit, nil, nil))
	htlcTxWithChange.AddTxOut(wire.NewTxOut(500, changePkScript))
	htlcTxWithChange.AddTxOut(wire.NewTxOut(1000, htlcPkScript))

	tests := []struct {
		name  string
		txs   []lndclient.Transaction
		spent bool
	}{{
		name: "htlc tx spends the deposit",
		txs: []lndclient.Transaction{{
			Tx: spendingTx(deposit, htlcPkScript),
		}},
		spent: true,
	}, {
		name: "other tx spends the deposit",
		txs: []lndclient.Transaction{{
			Tx: spendingTx(deposit, []byte{0x00, 0x14}),
		}},
	}, {
		name: "tx pays to the htlc without spending the deposit",
		txs: []lndclient.Transaction{{
			Tx: spendingTx(otherOutpoint, htlcPkScript),
		}},
	}, {
		name: "htlc tx listed after unrelated transactions",
		txs: []lndclient.Transaction{{
			Tx: spendingTx(otherOutpoint, []byte{0x00, 0x14}),
		}, {
			Tx: spendingTx(deposit, []byte{0x00, 0x14}),
		}, {
			Tx: spendingTx(deposit, htlcPkScript),
		}},
		spent: true,
	}, {
		name: "later input spends a later deposit, htlc after change",
		txs: []lndclient.Transaction{{
			Tx: htlcTxWithChange,
		}},
		spent: true,
	}, {
		name: "no wallet transactions",
	}, {
		// lndclient fails the whole call if it can't parse a raw tx,
		// so this only guards against a nil Tx from other clients.
		name: "transaction without raw tx",
		txs: []lndclient.Transaction{{
			PreviousOutpoints: []*lnrpc.PreviousOutPoint{{
				Outpoint: deposit.String(),
			}},
		}},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockTxListLightningClient{txs: tc.txs}
			checker := NewLndTxOutChecker(client)

			spent, err := checker.SpentByTxPayingTo(
				t.Context(),
				[]wire.OutPoint{deposit, secondDeposit},
				htlcPkScript, startHeight,
			)
			require.NoError(t, err)
			require.Equal(t, tc.spent, spent)
			require.Equal(t, []txListCall{{
				startHeight: startHeight,
				endHeight:   -1,
			}}, client.calls)
		})
	}

	t.Run("returns error", func(t *testing.T) {
		expectedErr := errors.New("list transactions failed")
		client := &mockTxListLightningClient{err: expectedErr}
		checker := NewLndTxOutChecker(client)

		_, err := checker.SpentByTxPayingTo(
			t.Context(), []wire.OutPoint{deposit}, htlcPkScript,
			startHeight,
		)
		require.ErrorIs(t, err, expectedErr)
	})
}

// TestDepositSpendProof requires positive confirmed conflict evidence. Missing
// history, mempool spends and the swap's own HTLC never justify abandonment.
func TestDepositSpendProof(t *testing.T) {
	outpoint := wire.OutPoint{Index: 3}
	htlcScript := []byte{0, 32, 1, 2, 3}
	for _, test := range []struct {
		name          string
		confirmations int32
		paysHtlc      bool
		missing       bool
		want          bool
	}{
		{"confirmed unrelated spend", defaultConfTarget, false, false, true},
		{"unconfirmed conflict", 0, false, false, false},
		{"shallow conflict", defaultConfTarget - 1, false, false, false},
		{"historical htlc", defaultConfTarget, true, false, false},
		{"missing funding", 0, false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := wire.NewMsgTx(2)
			tx.AddTxIn(&wire.TxIn{PreviousOutPoint: outpoint})
			script := []byte{0x51}
			if test.paysHtlc {
				script = htlcScript
			}
			tx.AddTxOut(wire.NewTxOut(1_000, script))
			client := &mockTxListLightningClient{}
			if !test.missing {
				client.txs = []lndclient.Transaction{{
					Tx: tx, Confirmations: test.confirmations,
				}}
			}
			checker := &lndTxOutChecker{client: client}
			outputs, conflict, err := checker.CheckDepositSpend(
				t.Context(), []wire.OutPoint{outpoint}, htlcScript, nil,
			)
			require.NoError(t, err)
			require.Empty(t, outputs)
			require.Equal(t, test.want, conflict)
		})
	}
}

type txListCall struct {
	startHeight int32
	endHeight   int32
}

type mockTxListLightningClient struct {
	lndclient.LightningClient

	txs     []lndclient.Transaction
	err     error
	calls   []txListCall
	infos   []*lndclient.Info
	infoErr error
	listed  chan struct{}
}

func (m *mockTxListLightningClient) GetInfo(context.Context) (*lndclient.Info, error) {
	if m.infoErr != nil {
		return nil, m.infoErr
	}
	if len(m.infos) > 0 {
		info := m.infos[0]
		m.infos = m.infos[1:]
		return info, nil
	}
	return &lndclient.Info{BlockHeight: 100, SyncedToChain: true}, nil
}

func (m *mockTxListLightningClient) ListTransactions(_ context.Context,
	startHeight, endHeight int32, _ ...lndclient.ListTransactionsOption) (
	[]lndclient.Transaction, error) {

	m.calls = append(m.calls, txListCall{
		startHeight: startHeight,
		endHeight:   endHeight,
	})
	if m.listed != nil {
		m.listed <- struct{}{}
	}

	return m.txs, m.err
}

// TestHtlcTxSearchStartHeight checks that the htlc tx lookup starts
// htlcTxReorgLookback blocks below the initiation height, clamped to zero.
func TestHtlcTxSearchStartHeight(t *testing.T) {
	tests := []struct {
		initiationHeight uint32
		startHeight      int32
	}{
		{initiationHeight: 0, startHeight: 0},
		{initiationHeight: htlcTxReorgLookback, startHeight: 0},
		{initiationHeight: htlcTxReorgLookback + 1, startHeight: 1},
		{initiationHeight: 900_000, startHeight: 900_000 - 144},
	}

	for _, tc := range tests {
		require.Equal(
			t, tc.startHeight,
			htlcTxSearchStartHeight(tc.initiationHeight),
		)
	}
}
