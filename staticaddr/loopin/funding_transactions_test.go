package loopin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/require"
)

// TestInitFundingSnapshotFailure never advances to signing when funding cannot
// be captured or persisted, and preserves the error when abandonment succeeds.
func TestInitFundingSnapshotFailure(t *testing.T) {
	for _, missing := range []bool{true, false} {
		name := "snapshot persistence failure"
		if missing {
			name = "missing funding history"
		}
		t.Run(name, func(t *testing.T) {
			lnd := test.NewMockLnd()
			_, clientKey := test.CreateKey(111)
			_, serverKey := test.CreateKey(112)
			dep := &deposit.Deposit{Hash: chainhash.Hash{1}, Value: 500_000,
				AddressParams: &script.Parameters{
					ClientPubkey: clientKey, PkScript: []byte{0x51}, Expiry: 2_000,
				}}
			if !missing {
				addTestFundingTransactions(lnd, dep)
			}
			server := &initHtlcTestServer{loopInResp: testStaticAddressLoopInResponse(serverKey.SerializeCompressed())}
			storeErr := errors.New("snapshot write failed")
			store := &fundingSnapshotFailureStore{err: storeErr}
			f := &FSM{StateMachine: &fsm.StateMachine{},
				loopIn: &StaticAddressLoopIn{
					Deposits:         []*deposit.Deposit{dep},
					DepositOutpoints: []string{dep.OutPoint.String()},
					QuotedSwapFee:    1_000, InitiationHeight: uint32(lnd.Height),
					InitiationTime: time.Now(),
				},
				cfg: &Config{
					Server: server, Store: store,
					DepositManager: &noopDepositManager{},
					LndClient:      lnd.Client, InvoicesClient: lnd.LndServices.Invoices,
					WalletKit: lnd.WalletKit, ChainParams: lnd.ChainParams,
					ValidateLoopInContract:               testValidateLoopInContract,
					MaxStaticAddrHtlcFeePercentage:       1,
					MaxStaticAddrHtlcBackupFeePercentage: 1,
				}}
			require.Equal(t, fsm.OnError, f.InitHtlcAction(t.Context(), nil))
			if missing {
				require.ErrorContains(t, f.LastActionError, "unconfirmed funding transactions")
				require.False(t, store.called)
			} else {
				require.ErrorIs(t, f.LastActionError, storeErr)
				require.True(t, store.called)
				require.Len(t, f.loopIn.FundingTxs, 1)
			}
			require.Len(t, server.pushedSigs, 1)
			require.Nil(t, server.pushedSigs[0].StandardHtlcInfo)
			require.Nil(t, server.pushedSigs[0].HighFeeHtlcInfo)
			require.Nil(t, server.pushedSigs[0].ExtremeFeeHtlcInfo)
			require.Equal(t, f.loopIn.SwapHash, <-lnd.FailInvoiceChannel)
		})
	}
}

type fundingSnapshotFailureStore struct {
	mockStore

	err    error
	called bool
}

func (s *fundingSnapshotFailureStore) CreateLoopIn(context.Context,
	*StaticAddressLoopIn) error {

	s.called = true
	return s.err
}

// TestFundingTransactions verifies that retained ancestry matches the original
// unconfirmed deposits, including multiple outputs of one funding transaction.
func TestFundingTransactions(t *testing.T) {
	parent := wire.NewMsgTx(2)
	parent.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
		Hash: chainhash.Hash{1},
	}})
	parent.AddTxOut(wire.NewTxOut(100_000, []byte{0x51}))
	parent.AddTxOut(wire.NewTxOut(200_000, []byte{0x52}))

	for _, test := range []struct {
		name string
		edit func(*deposit.Deposit, *mockTxListLightningClient)
		fail bool
	}{
		{name: "shared parent"},
		{name: "negative confirmation height", edit: func(d *deposit.Deposit,
			_ *mockTxListLightningClient) {

			d.ConfirmationHeight = -1
		}},
		{name: "missing history", fail: true, edit: func(_ *deposit.Deposit,
			c *mockTxListLightningClient) {

			c.txs = nil
		}},
		{name: "lookup error", fail: true, edit: func(_ *deposit.Deposit,
			c *mockTxListLightningClient) {

			c.err = errors.New("wallet unavailable")
		}},
		{name: "wrong raw transaction hash", fail: true,
			edit: func(_ *deposit.Deposit, c *mockTxListLightningClient) {
				c.txs[0].Tx = parent.Copy()
				c.txs[0].Tx.TxOut[0].Value++
			}},
		{name: "wrong value", fail: true, edit: func(d *deposit.Deposit,
			_ *mockTxListLightningClient) {

			d.Value++
		}},
		{name: "wrong script", fail: true, edit: func(d *deposit.Deposit,
			_ *mockTxListLightningClient) {

			d.AddressParams.PkScript = []byte{0x53}
		}},
		{name: "wrong output index", fail: true, edit: func(d *deposit.Deposit,
			_ *mockTxListLightningClient) {

			d.Index = 2
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := &deposit.Deposit{
				Hash: parent.TxHash(), Value: 100_000,
				AddressParams: &script.Parameters{PkScript: []byte{0x51}},
			}
			second := &deposit.Deposit{
				Hash: parent.TxHash(), Index: 1, Value: 200_000,
				AddressParams: &script.Parameters{PkScript: []byte{0x52}},
			}
			client := &mockTxListLightningClient{
				txs: []lndclient.Transaction{{Tx: parent,
					TxHash: parent.TxHash().String()}},
			}
			if test.edit != nil {
				test.edit(first, client)
			}
			funding, err := fundingTransactions(
				t.Context(), client, []*deposit.Deposit{first, second},
			)
			if test.fail {
				require.Error(t, err)
				require.Nil(t, funding)
				return
			}
			require.NoError(t, err)
			require.Len(t, funding, 1)
			require.Equal(t, parent, funding[0])
			funding[0].TxOut[0].Value++
			require.EqualValues(t, 100_000, parent.TxOut[0].Value)
		})
	}

	t.Run("confirmed inputs need no snapshot", func(t *testing.T) {
		funding, err := fundingTransactions(t.Context(), nil,
			[]*deposit.Deposit{{ConfirmationHeight: 100}})
		require.NoError(t, err)
		require.Nil(t, funding)
	})
}

// addTestFundingTransactions gives action fixtures real unconfirmed parents
// so initialization exercises ancestry capture before persisting a swap.
func addTestFundingTransactions(lnd *test.LndMockServices,
	deposits ...*deposit.Deposit) {

	for _, d := range deposits {
		parent := wire.NewMsgTx(2)
		parent.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
			Hash: d.Hash, Index: d.Index,
		}})
		for range d.Index {
			parent.AddTxOut(wire.NewTxOut(0, []byte{0x6a}))
		}
		parent.AddTxOut(wire.NewTxOut(int64(d.Value), d.AddressParams.PkScript))
		d.Hash = parent.TxHash()
		lnd.Transactions = append(lnd.Transactions, lndclient.Transaction{
			Tx: parent, TxHash: parent.TxHash().String(),
		})
	}
}
