package reservation

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

type confirmationNode struct {
	lndclient.LightningClient
}

func (confirmationNode) GetInfo(context.Context) (*lndclient.Info, error) {
	return &lndclient.Info{
		SyncedToChain: true,
		BlockHeight:   102,
	}, nil
}

type confirmationNotifier struct {
	lndclient.ChainNotifierClient

	t        *testing.T
	tx       *wire.MsgTx
	outpoint wire.OutPoint
	spent    bool
	spends   chan *chainntnfs.SpendDetail
	blocks   chan int32
}

func (n *confirmationNotifier) RegisterConfirmationsNtfn(_ context.Context,
	txid *chainhash.Hash, script []byte, depth, hint int32,
	_ ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

	require.Equal(n.t, n.tx.TxHash(), *txid)
	require.Equal(n.t, n.tx.TxOut[1].PkScript, script)
	require.EqualValues(n.t, 1, depth)
	require.EqualValues(n.t, 99, hint)
	confs := make(chan *chainntnfs.TxConfirmation, 1)
	confs <- &chainntnfs.TxConfirmation{
		Tx:          n.tx,
		BlockHeight: 100,
	}
	return confs, make(chan error), nil
}

func (n *confirmationNotifier) RegisterSpendNtfn(_ context.Context,
	outpoint *wire.OutPoint, _ []byte, hint int32,
	_ ...lndclient.NotifierOption) (chan *chainntnfs.SpendDetail,
	chan error, error) {

	require.Equal(n.t, n.outpoint, *outpoint)
	require.EqualValues(n.t, 99, hint)
	if n.spends != nil {
		return n.spends, make(chan error), nil
	}
	spends := make(chan *chainntnfs.SpendDetail, 1)
	if n.spent {
		spends <- &chainntnfs.SpendDetail{
			SpentOutPoint: outpoint,
		}
	}
	return spends, make(chan error), nil
}

func (n *confirmationNotifier) RegisterBlockEpochNtfn(context.Context) (
	chan int32, chan error, error) {

	return n.blocks, make(chan error), nil
}

// TestConfirmationObservation checks the exact output without wallet history.
func TestConfirmationObservation(t *testing.T) {
	const spentCase = "spent"
	for _, name := range []string{"confirmed", "wrong", spentCase} {
		t.Run(name, func(t *testing.T) {
			tx := wire.NewMsgTx(2)
			tx.AddTxOut(wire.NewTxOut(1000, []byte{0x51}))
			tx.AddTxOut(wire.NewTxOut(2000, []byte{0x52}))
			outpoint := wire.OutPoint{
				Hash:  tx.TxHash(),
				Index: 1,
			}
			reader := ChainReader{
				Chain: struct{ ChainSource }{},
				Node:  confirmationNode{},
				ChainNotifier: &confirmationNotifier{
					t:        t,
					tx:       tx,
					outpoint: outpoint,
					spent:    name == spentCase,
				},
			}
			output := *tx.TxOut[1]
			if name == "wrong" {
				output.Value++
			}
			status, err := reader.Observe(
				t.Context(), outpoint, &output, 99,
			)
			switch name {
			case "wrong":
				require.ErrorIs(t, err, ErrInvalidReservation)

			default:
				require.NoError(t, err)
				require.EqualValues(t, 100, status.ConfirmationHeight)
				require.EqualValues(t, 102, status.Height)
				require.Equal(t, name == spentCase, status.Spent)
			}
		})
	}
}

// TestReservationWatchRecovery receives a historical spend after the initial
// block snapshot, without requiring a wallet import or a scan cursor.
func TestReservationWatchRecovery(t *testing.T) {
	point := wire.OutPoint{
		Index: 2,
	}
	for range 2 {
		spends := make(chan *chainntnfs.SpendDetail)
		blocks := make(chan int32)
		reader := ChainReader{
			Chain: struct{ ChainSource }{},
			Node:  confirmationNode{},
			ChainNotifier: &confirmationNotifier{
				t:        t,
				outpoint: point,
				spends:   spends,
				blocks:   blocks,
			},
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			select {
			case blocks <- 102:

			case <-ctx.Done():
				return
			}
			select {
			case spends <- &chainntnfs.SpendDetail{
				SpentOutPoint: &point,
			}:

			case <-ctx.Done():
			}
		}()
		status, err := reader.WaitForChange(ctx, point, []byte{0x51}, 99,
			FundingStatus{
				Height:             102,
				ConfirmationHeight: 100,
			})
		cancel()
		<-finished
		require.NoError(t, err)
		require.True(t, status.Spent)
	}
}

func TestReservationWatchIdleDeadline(t *testing.T) {
	point := wire.OutPoint{Index: 2}
	reader := ChainReader{
		Chain: struct{ ChainSource }{}, Node: confirmationNode{},
		ChainNotifier: &confirmationNotifier{
			t: t, outpoint: point,
			spends: make(chan *chainntnfs.SpendDetail), blocks: make(chan int32),
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	initial := FundingStatus{Height: 102, ConfirmationHeight: 100}
	result, err := reader.WaitForChange(ctx, point, []byte{0x51}, 99, initial)
	require.NoError(t, err)
	require.Equal(t, initial, result)
}
