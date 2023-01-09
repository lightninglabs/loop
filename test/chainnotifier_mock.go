package test

import (
	"bytes"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"golang.org/x/net/context"
)

type mockChainNotifier struct {
	sync.Mutex
	lnd               *LndMockServices
	confRegistrations []*ConfRegistration
	wg                sync.WaitGroup
}

// SpendRegistration contains registration details.
type SpendRegistration struct {
	Outpoint   *wire.OutPoint
	PkScript   []byte
	HeightHint int32
}

// ConfRegistration contains registration details.
type ConfRegistration struct {
	TxID       *chainhash.Hash
	PkScript   []byte
	HeightHint int32
	NumConfs   int32
	ConfChan   chan *chainntnfs.TxConfirmation
}

func (c *mockChainNotifier) RegisterSpendNtfn(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint int32) (
	chan *chainntnfs.SpendDetail, chan error, error) {

	c.lnd.RegisterSpendChannel <- &SpendRegistration{
		HeightHint: heightHint,
		Outpoint:   outpoint,
		PkScript:   pkScript,
	}

	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	errChan := make(chan error, 1)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		select {
		case m := <-c.lnd.SpendChannel:
			select {
			case spendChan <- m:
			case <-ctx.Done():
			}
		case <-ctx.Done():
		}
	}()

	return spendChan, errChan, nil
}

func (c *mockChainNotifier) WaitForFinished() {
	c.wg.Wait()
}

func (c *mockChainNotifier) RegisterBlockEpochNtfn(ctx context.Context) (
	chan int32, chan error, error) {

	blockErrorChan := make(chan error, 1)
	blockEpochChan := make(chan int32)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		// Send initial block height
		select {
		case blockEpochChan <- c.lnd.Height:
		case <-ctx.Done():
			return
		}

		for {
			select {
			case m := <-c.lnd.epochChannel:
				select {
				case blockEpochChan <- m:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return blockEpochChan, blockErrorChan, nil
}

func (c *mockChainNotifier) RegisterConfirmationsNtfn(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs, heightHint int32,
	opts ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

	reg := &ConfRegistration{
		PkScript:   pkScript,
		TxID:       txid,
		HeightHint: heightHint,
		NumConfs:   numConfs,
		ConfChan:   make(chan *chainntnfs.TxConfirmation, 1),
	}

	c.Lock()
	c.confRegistrations = append(c.confRegistrations, reg)
	c.Unlock()

	errChan := make(chan error, 1)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		select {
		case m := <-c.lnd.ConfChannel:
			c.Lock()
			for i := 0; i < len(c.confRegistrations); i++ {
				r := c.confRegistrations[i]

				// Whichever conf notifier catches the confirmation
				// will forward it to all matching subscibers.
				if bytes.Equal(m.Tx.TxOut[0].PkScript, r.PkScript) {
					// Unregister the "notifier".
					c.confRegistrations = append(
						c.confRegistrations[:i], c.confRegistrations[i+1:]...,
					)
					i--

					select {
					case r.ConfChan <- m:
					case <-ctx.Done():
					}
				}
			}
			c.Unlock()
		case <-ctx.Done():
		}
	}()

	select {
	case c.lnd.RegisterConfChannel <- reg:
	case <-time.After(Timeout):
		return nil, nil, ErrTimeout
	}

	return reg.ConfChan, errChan, nil
}
