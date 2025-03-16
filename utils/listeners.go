package utils

import (
	"context"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lntypes"
)

// ExpiryManager is a manager for block height expiry events.
type ExpiryManager struct {
	chainNotifier lndclient.ChainNotifierClient

	expiryHeightMap map[[32]byte]int32
	expiryFuncMap   map[[32]byte]func()

	currentBlockHeight int32

	sync.Mutex
}

// NewExpiryManager creates a new expiry manager.
func NewExpiryManager(
	chainNotifier lndclient.ChainNotifierClient) *ExpiryManager {

	return &ExpiryManager{
		chainNotifier:   chainNotifier,
		expiryHeightMap: make(map[[32]byte]int32),
		expiryFuncMap:   make(map[[32]byte]func()),
	}
}

// Start starts the expiry manager and listens for block height notifications.
func (e *ExpiryManager) Start(ctx context.Context, startingBlockHeight int32,
) error {

	e.Lock()
	e.currentBlockHeight = startingBlockHeight
	e.Unlock()

	log.Debugf("Starting expiry manager at height %d", startingBlockHeight)
	defer log.Debugf("Expiry manager stopped")

	blockHeightChan, errChan, err := e.chainNotifier.RegisterBlockEpochNtfn(
		ctx,
	)
	if err != nil {
		return err
	}

	for {
		select {
		case blockHeight := <-blockHeightChan:

			log.Debugf("Received block height %d", blockHeight)

			e.Lock()
			e.currentBlockHeight = blockHeight
			e.Unlock()

			e.checkExpiry(blockHeight)

		case err := <-errChan:
			log.Debugf("Expiry manager error")
			return err

		case <-ctx.Done():
			log.Debugf("Expiry manager stopped")
			return nil
		}
	}
}

// GetBlockHeight returns the current block height.
func (e *ExpiryManager) GetBlockHeight() int32 {
	e.Lock()
	defer e.Unlock()

	return e.currentBlockHeight
}

// checkExpiry checks if any swaps have expired and calls the expiry function if
// they have.
func (e *ExpiryManager) checkExpiry(blockHeight int32) {
	e.Lock()
	defer e.Unlock()

	for swapHash, expiryHeight := range e.expiryHeightMap {
		if blockHeight >= expiryHeight {
			expiryFunc := e.expiryFuncMap[swapHash]
			go expiryFunc()

			delete(e.expiryHeightMap, swapHash)
			delete(e.expiryFuncMap, swapHash)
		}
	}
}

// SubscribeExpiry subscribes to an expiry event for a swap. If the expiry height
// has already been reached, the expiryFunc is not called and the function
// returns true. Otherwise, the expiryFunc is called when the expiry height is
// reached and the function returns false.
func (e *ExpiryManager) SubscribeExpiry(swapHash [32]byte,
	expiryHeight int32, expiryFunc func()) bool {

	e.Lock()
	defer e.Unlock()

	if e.currentBlockHeight >= expiryHeight {
		return true
	}

	log.Debugf("Subscribing to expiry for swap %x at height %d",
		swapHash, expiryHeight)

	e.expiryHeightMap[swapHash] = expiryHeight
	e.expiryFuncMap[swapHash] = expiryFunc

	return false
}

// SubscribeInvoiceManager is a manager for invoice subscription events.
type SubscribeInvoiceManager struct {
	invoicesClient lndclient.InvoicesClient

	subscribers map[[32]byte]struct{}

	sync.Mutex
}

// NewSubscribeInvoiceManager creates a new subscribe invoice manager.
func NewSubscribeInvoiceManager(
	invoicesClient lndclient.InvoicesClient) *SubscribeInvoiceManager {

	return &SubscribeInvoiceManager{
		invoicesClient: invoicesClient,
		subscribers:    make(map[[32]byte]struct{}),
	}
}

// SubscribeInvoice subscribes to invoice events for a swap hash. The update
// callback is called when the invoice is updated and the error callback is
// called when an error occurs.
func (s *SubscribeInvoiceManager) SubscribeInvoice(ctx context.Context,
	invoiceHash lntypes.Hash, callback func(lndclient.InvoiceUpdate, error),
) error {

	s.Lock()
	defer s.Unlock()
	// If we already have a subscriber for this swap hash, return early.
	if _, ok := s.subscribers[invoiceHash]; ok {
		return nil
	}

	log.Debugf("Subscribing to invoice %v", invoiceHash)

	updateChan, errChan, err := s.invoicesClient.SubscribeSingleInvoice(
		ctx, invoiceHash,
	)
	if err != nil {
		return err
	}

	s.subscribers[invoiceHash] = struct{}{}

	go func() {
		for {
			select {
			case update := <-updateChan:
				callback(update, nil)

			case err := <-errChan:
				callback(lndclient.InvoiceUpdate{}, err)
				delete(s.subscribers, invoiceHash)
				return

			case <-ctx.Done():
				delete(s.subscribers, invoiceHash)
				return
			}
		}
	}()

	return nil
}

// TxSubscribeConfirmationManager is a manager for transaction confirmation
// subscription events.
type TxSubscribeConfirmationManager struct {
	chainNotifier lndclient.ChainNotifierClient

	subscribers map[[32]byte]struct{}

	sync.Mutex
}

// NewTxSubscribeConfirmationManager creates a new transaction confirmation
// subscription manager.
func NewTxSubscribeConfirmationManager(chainNtfn lndclient.ChainNotifierClient,
) *TxSubscribeConfirmationManager {

	return &TxSubscribeConfirmationManager{
		chainNotifier: chainNtfn,
		subscribers:   make(map[[32]byte]struct{}),
	}
}

// SubscribeTxConfirmation subscribes to transaction confirmation events for a
// swap hash. The callback is called when the transaction is confirmed or an
// error occurs.
func (t *TxSubscribeConfirmationManager) SubscribeTxConfirmation(
	ctx context.Context, swapHash lntypes.Hash, txid *chainhash.Hash,
	pkscript []byte, numConfs int32, heightHint int32,
	cb func(*chainntnfs.TxConfirmation, error)) error {

	t.Lock()
	defer t.Unlock()

	// If we already have a subscriber for this swap hash, return early.
	if _, ok := t.subscribers[swapHash]; ok {
		return nil
	}

	log.Debugf("Subscribing to tx confirmation for swap %v", swapHash)

	confChan, errChan, err := t.chainNotifier.RegisterConfirmationsNtfn(
		ctx, txid, pkscript, numConfs, heightHint,
	)
	if err != nil {
		return err
	}

	t.subscribers[swapHash] = struct{}{}

	go func() {
		for {
			select {
			case conf := <-confChan:
				cb(conf, nil)

			case err := <-errChan:
				cb(nil, err)
				delete(t.subscribers, swapHash)
				return

			case <-ctx.Done():
				delete(t.subscribers, swapHash)
				return
			}
		}
	}()

	return nil
}
