package loopin

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/stretchr/testify/require"
)

// TestFundingConflictProof distinguishes a mined funding replacement from
// missing history, mempool conflicts and the original transaction itself.
func TestFundingConflictProof(t *testing.T) {
	htlcScript := []byte{0x51, 0x20, 1}
	for _, test := range []struct {
		name       string
		edit       func(*wire.MsgTx, *mockTxListLightningClient)
		noSnapshot bool
		want       bool
		wantErr    bool
	}{
		{name: "confirmed replacement", want: true},
		{name: "legacy missing snapshot", noSnapshot: true},
		{name: "funding and htlc removed from wallet", edit: func(_ *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.txs = nil
		}},
		{name: "unconfirmed replacement", edit: func(_ *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.txs[0].Confirmations = 0
		}},
		{name: "shallow replacement", edit: func(_ *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.txs[0].Confirmations = defaultConfTarget - 1
		}},
		{name: "original funding confirms", edit: func(funding *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.txs[0].Tx = funding.Copy()
		}},
		{name: "witness change is same original", edit: func(funding *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.txs[0].Tx = funding.Copy()
			c.txs[0].Tx.TxIn[0].Witness = wire.TxWitness{{1, 2, 3}}
		}},
		{name: "independent payment to same address", edit: func(_ *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.txs[0].Tx.TxIn[0].PreviousOutPoint.Hash = chainhash.Hash{3}
		}},
		{name: "htlc paying replacement stays monitored", edit: func(_ *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.txs[0].Tx.TxOut[0].PkScript = htlcScript
		}},
		{name: "wallet unsynced", edit: func(_ *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.infos = []*lndclient.Info{{BlockHeight: 100},
				{BlockHeight: 100, SyncedToChain: true}}
		}},
		{name: "wallet tip changed", edit: func(_ *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.infos = []*lndclient.Info{{BlockHeight: 100, SyncedToChain: true},
				{BlockHeight: 101, SyncedToChain: true}}
		}},
		{name: "same height reorg", edit: func(_ *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.infos = []*lndclient.Info{{BlockHeight: 100, SyncedToChain: true,
				BestBlockHash: chainhash.Hash{1}}, {BlockHeight: 100,
				SyncedToChain: true, BestBlockHash: chainhash.Hash{2}}}
		}},
		{name: "wallet error", wantErr: true, edit: func(_ *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.err = errors.New("wallet unavailable")
		}},
		{name: "tip error", wantErr: true, edit: func(_ *wire.MsgTx,
			c *mockTxListLightningClient) {

			c.infoErr = errors.New("tip unavailable")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			funding := wire.NewMsgTx(2)
			funding.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.Hash{1},
			}})
			funding.AddTxOut(wire.NewTxOut(200_000, []byte{0x51}))
			replacement := funding.Copy()
			replacement.TxOut[0].Value--
			client := &mockTxListLightningClient{
				txs: []lndclient.Transaction{{Tx: replacement,
					Confirmations: defaultConfTarget}},
			}
			if test.edit != nil {
				test.edit(funding, client)
			}
			var retained []*wire.MsgTx
			if !test.noSnapshot {
				retained = []*wire.MsgTx{funding}
			}
			_, conflict, err := (&lndTxOutChecker{client: client}).CheckDepositSpend(
				t.Context(), []wire.OutPoint{{Hash: funding.TxHash()}}, htlcScript,
				retained,
			)
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, test.want, conflict)
		})
	}
}

// TestInvalidFundingSnapshots rejects coinbase and duplicate inputs instead
// of treating their common or malformed prevouts as positive conflict evidence.
func TestInvalidFundingSnapshots(t *testing.T) {
	for _, test := range []struct {
		name string
		tx   *wire.MsgTx
	}{
		{name: "nil"},
		{name: "empty", tx: wire.NewMsgTx(2)},
		{name: "null prevout", tx: &wire.MsgTx{Version: 2,
			TxIn:  []*wire.TxIn{{PreviousOutPoint: wire.OutPoint{Index: ^uint32(0)}}},
			TxOut: []*wire.TxOut{wire.NewTxOut(1, []byte{0x51})}}},
		{name: "repeated funding input", tx: &wire.MsgTx{Version: 2,
			TxIn:  []*wire.TxIn{{}, {}},
			TxOut: []*wire.TxOut{wire.NewTxOut(1, []byte{0x51})}}},
		{name: "nil input", tx: &wire.MsgTx{Version: 2,
			TxIn:  []*wire.TxIn{nil},
			TxOut: []*wire.TxOut{wire.NewTxOut(1, []byte{0x51})}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, validateFundingTransaction(test.tx))
		})
	}
}
