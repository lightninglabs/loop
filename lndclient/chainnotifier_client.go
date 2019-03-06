package lndclient

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ChainNotifierClient exposes base lightning functionality.
type ChainNotifierClient interface {
	RegisterBlockEpochNtfn(ctx context.Context) (
		chan int32, chan error, error)

	RegisterConfirmationsNtfn(ctx context.Context, txid *chainhash.Hash,
		pkScript []byte, numConfs, heightHint int32) (
		chan *chainntnfs.TxConfirmation, chan error, error)

	RegisterSpendNtfn(ctx context.Context,
		outpoint *wire.OutPoint, pkScript []byte, heightHint int32) (
		chan *chainntnfs.SpendDetail, chan error, error)
}

type chainNotifierClient struct {
	client chainrpc.ChainNotifierClient
	wg     sync.WaitGroup
}

func newChainNotifierClient(conn *grpc.ClientConn) *chainNotifierClient {
	return &chainNotifierClient{
		client: chainrpc.NewChainNotifierClient(conn),
	}
}

func (s *chainNotifierClient) WaitForFinished() {
	s.wg.Wait()
}

func (s *chainNotifierClient) RegisterSpendNtfn(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint int32) (
	chan *chainntnfs.SpendDetail, chan error, error) {

	var rpcOutpoint *chainrpc.Outpoint
	if outpoint != nil {
		rpcOutpoint = &chainrpc.Outpoint{
			Hash:  outpoint.Hash[:],
			Index: outpoint.Index,
		}
	}

	resp, err := s.client.RegisterSpendNtfn(ctx, &chainrpc.SpendRequest{
		HeightHint: uint32(heightHint),
		Outpoint:   rpcOutpoint,
		Script:     pkScript,
	})
	if err != nil {
		return nil, nil, err
	}

	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	errChan := make(chan error, 1)

	processSpendDetail := func(d *chainrpc.SpendDetails) error {
		outpointHash, err := chainhash.NewHash(d.SpendingOutpoint.Hash)
		if err != nil {
			return err
		}
		txHash, err := chainhash.NewHash(d.SpendingTxHash)
		if err != nil {
			return err
		}
		tx, err := utils.DecodeTx(d.RawSpendingTx)
		if err != nil {
			return err
		}
		spendChan <- &chainntnfs.SpendDetail{
			SpentOutPoint: &wire.OutPoint{
				Hash:  *outpointHash,
				Index: d.SpendingOutpoint.Index,
			},
			SpenderTxHash:     txHash,
			SpenderInputIndex: d.SpendingInputIndex,
			SpendingTx:        tx,
			SpendingHeight:    int32(d.SpendingHeight),
		}

		return nil
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			spendEvent, err := resp.Recv()
			if err != nil {
				if status.Code(err) != codes.Canceled {
					errChan <- err
				}
				return
			}

			switch c := spendEvent.Event.(type) {
			case *chainrpc.SpendEvent_Spend:
				err := processSpendDetail(c.Spend)
				if err != nil {
					errChan <- err
				}
				return
			}
		}
	}()

	return spendChan, errChan, nil
}

func (s *chainNotifierClient) RegisterConfirmationsNtfn(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs, heightHint int32) (
	chan *chainntnfs.TxConfirmation, chan error, error) {

	// TODO: Height hint
	var txidSlice []byte
	if txid != nil {
		txidSlice = txid[:]
	}
	confStream, err := s.client.
		RegisterConfirmationsNtfn(
			ctx,
			&chainrpc.ConfRequest{
				Script:     pkScript,
				NumConfs:   uint32(numConfs),
				HeightHint: uint32(heightHint),
				Txid:       txidSlice,
			},
		)
	if err != nil {
		return nil, nil, err
	}

	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	errChan := make(chan error, 1)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		for {
			var confEvent *chainrpc.ConfEvent
			confEvent, err := confStream.Recv()
			if err != nil {
				if status.Code(err) != codes.Canceled {
					errChan <- err
				}
				return
			}

			switch c := confEvent.Event.(type) {

			// Script confirmed
			case *chainrpc.ConfEvent_Conf:
				tx, err := utils.DecodeTx(c.Conf.RawTx)
				if err != nil {
					errChan <- err
					return
				}
				blockHash, err := chainhash.NewHash(
					c.Conf.BlockHash,
				)
				if err != nil {
					errChan <- err
					return
				}
				confChan <- &chainntnfs.TxConfirmation{
					BlockHeight: c.Conf.BlockHeight,
					BlockHash:   blockHash,
					Tx:          tx,
					TxIndex:     c.Conf.TxIndex,
				}
				return

			// Ignore reorg events, not supported.
			case *chainrpc.ConfEvent_Reorg:
				continue

			// Nil event, should never happen.
			case nil:
				errChan <- fmt.Errorf("conf event empty")
				return

			// Unexpected type.
			default:
				errChan <- fmt.Errorf(
					"conf event has unexpected type",
				)
				return
			}
		}
	}()

	return confChan, errChan, nil
}

func (s *chainNotifierClient) RegisterBlockEpochNtfn(ctx context.Context) (
	chan int32, chan error, error) {

	blockEpochClient, err := s.client.
		RegisterBlockEpochNtfn(ctx, &chainrpc.BlockEpoch{})
	if err != nil {
		return nil, nil, err
	}

	blockErrorChan := make(chan error, 1)
	blockEpochChan := make(chan int32)

	// Start block epoch goroutine.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			epoch, err := blockEpochClient.Recv()
			if err != nil {
				if status.Code(err) != codes.Canceled {
					blockErrorChan <- err
				}
				return
			}

			select {
			case blockEpochChan <- int32(epoch.Height):
			case <-ctx.Done():
				return
			}
		}
	}()

	return blockEpochChan, blockErrorChan, nil
}
