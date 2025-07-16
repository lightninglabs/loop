package test

import (
	"bytes"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"golang.org/x/net/context"
)

type mockChainNotifier struct {
	lndclient.ChainNotifierClient

	sync.Mutex
	lnd               *LndMockServices
	confRegistrations []*ConfRegistration
	wg                sync.WaitGroup
}

var _ lndclient.ChainNotifierClient = (*mockChainNotifier)(nil)

func (c *mockChainNotifier) RawClientWithMacAuth(
	ctx context.Context) (context.Context, time.Duration,
	chainrpc.ChainNotifierClient) {

	return ctx, 0, nil
}

// SpendRegistration contains registration details.
type SpendRegistration struct {
	Outpoint     *wire.OutPoint
	PkScript     []byte
	HeightHint   int32
	SpendChannel chan<- *chainntnfs.SpendDetail
	ErrChan      chan<- error
}

// ConfRegistration contains registration details.
type ConfRegistration struct {
	TxID       *chainhash.Hash
	PkScript   []byte
	HeightHint int32
	NumConfs   int32
	ConfChan   chan *chainntnfs.TxConfirmation
	ErrChan    chan<- error
}

func (c *mockChainNotifier) RegisterSpendNtfn(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint int32,
	_ ...lndclient.NotifierOption) (chan *chainntnfs.SpendDetail,
	chan error, error) {

	spendChan0 := make(chan *chainntnfs.SpendDetail)
	spendErrChan := make(chan error, 1)

	reg := &SpendRegistration{
		HeightHint:   heightHint,
		Outpoint:     outpoint,
		PkScript:     pkScript,
		SpendChannel: spendChan0,
		ErrChan:      spendErrChan,
	}

	c.lnd.RegisterSpendChannel <- reg

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

		case m := <-spendChan0:
			select {
			case spendChan <- m:
			case <-ctx.Done():
			}

		case err := <-spendErrChan:
			select {
			case errChan <- err:
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
	blockEpochChan := make(chan int32, 1)

	c.lnd.lock.Lock()
	c.lnd.blockHeightListeners = append(
		c.lnd.blockHeightListeners, blockEpochChan,
	)
	c.lnd.lock.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			c.lnd.lock.Lock()
			defer c.lnd.lock.Unlock()
			for i := 0; i < len(c.lnd.blockHeightListeners); i++ {
				if c.lnd.blockHeightListeners[i] == blockEpochChan {
					c.lnd.blockHeightListeners = append(
						c.lnd.blockHeightListeners[:i],
						c.lnd.blockHeightListeners[i+1:]...,
					)
					break
				}
			}
		}()

		// Send initial block height
		c.lnd.lock.Lock()
		select {
		case blockEpochChan <- c.lnd.Height:
		case <-ctx.Done():
		}
		c.lnd.lock.Unlock()

		<-ctx.Done()
	}()

	return blockEpochChan, blockErrorChan, nil
}

func (c *mockChainNotifier) RegisterConfirmationsNtfn(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs, heightHint int32,
	opts ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

	confErrChan := make(chan error, 1)

	reg := &ConfRegistration{
		PkScript:   pkScript,
		TxID:       txid,
		HeightHint: heightHint,
		NumConfs:   numConfs,
		ConfChan:   make(chan *chainntnfs.TxConfirmation, 1),
		ErrChan:    confErrChan,
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
				// will forward it to all matching subscribers.
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

		case err := <-confErrChan:
			select {
			case errChan <- err:
			case <-ctx.Done():
			}

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
