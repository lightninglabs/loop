package deposit

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/address"
)

var (
	// ErrManagerShuttingDown signals that the asset deposit manager is
	// shutting down and that no further calls should be made to it.
	ErrManagerShuttingDown = errors.New("asset deposit manager is " +
		"shutting down")
)

// DepositUpdateCallback is a callback that is called when a deposit state is
// updated. The callback receives the updated deposit info.
type DepositUpdateCallback func(*DepositInfo)

// Manager is responsible for creating, funding, sweeping and spending asset
// deposits used in asset swaps. It implements low level deposit management.
type Manager struct {
	// depositServiceClient is a deposit service client.
	depositServiceClient swapserverrpc.AssetDepositServiceClient

	// walletKit is the backing lnd wallet to use for deposit operations.
	walletKit lndclient.WalletKitClient

	// signer is the signer client of the backing lnd wallet.
	signer lndclient.SignerClient

	// chainNotifier is the chain notifier client of the underlyng lnd node.
	chainNotifier lndclient.ChainNotifierClient

	// tapClient is the tapd client handling the deposit assets.
	tapClient *assets.TapdClient

	// addressParams holds the TAP specific network params.
	addressParams address.ChainParams

	// store is the deposit SQL store.
	store *SQLStore

	// sweeper is responsible for assembling and publishing deposit sweeps.
	sweeper *Sweeper

	// currentHeight is the current block height of the chain.
	currentHeight uint32

	// deposits is a map of all active deposits. The key is the deposit ID.
	deposits map[string]*Deposit

	// subscribers is a map of all registered deposit update subscribers.
	// The key is the deposit ID.
	subscribers map[string][]DepositUpdateCallback

	// callEnter is used to sequentialize calls to the batch handler's
	// main event loop.
	callEnter chan struct{}

	// callLeave is used to resume the execution flow of the batch handler's
	// main event loop.
	callLeave chan struct{}

	// criticalErrChan is used to signal that a critical error has occurred
	// and that the manager should stop.
	criticalErrChan chan error

	// quit is owned by the parent batcher and signals that the batch must
	// stop.
	quit chan struct{}

	// runCtx is a function that returns the Manager's run-loop context.
	runCtx func() context.Context
}

// NewManager constructs a new asset deposit manager.
func NewManager(depositServiceClient swapserverrpc.AssetDepositServiceClient,
	walletKit lndclient.WalletKitClient, signer lndclient.SignerClient,
	chainNotifier lndclient.ChainNotifierClient,
	tapClient *assets.TapdClient, store *SQLStore,
	params *chaincfg.Params) *Manager {

	addressParams := address.ParamsForChain(params.Name)
	sweeper := NewSweeper(tapClient, walletKit, signer, addressParams)

	return &Manager{
		depositServiceClient: depositServiceClient,
		walletKit:            walletKit,
		signer:               signer,
		chainNotifier:        chainNotifier,
		tapClient:            tapClient,
		store:                store,
		sweeper:              sweeper,
		addressParams:        addressParams,
		deposits:             make(map[string]*Deposit),
		subscribers:          make(map[string][]DepositUpdateCallback),
		callEnter:            make(chan struct{}),
		callLeave:            make(chan struct{}),
		criticalErrChan:      make(chan error, 1),
		quit:                 make(chan struct{}),
	}
}

// Run is the entry point running that starts up the deposit manager and also
// runs the main event loop.
func (m *Manager) Run(ctx context.Context, bestBlock uint32) error {
	log.Infof("Starting asset deposit manager")
	defer log.Infof("Asset deposit manager stopped")

	ctxc, cancel := context.WithCancel(ctx)
	defer func() {
		// Signal to the main event loop that it should stop.
		close(m.quit)
		cancel()
	}()

	// Set the context getter.
	m.runCtx = func() context.Context {
		return ctxc
	}

	m.currentHeight = bestBlock

	blockChan, blockErrChan, err := m.chainNotifier.RegisterBlockEpochNtfn(
		ctxc,
	)
	if err != nil {
		log.Errorf("unable to register for block epoch notifications: "+
			"%v", err)

		return err
	}

	for {
		select {
		case <-m.callEnter:
			<-m.callLeave

		case blockHeight, ok := <-blockChan:
			if !ok {
				return nil
			}

			log.Debugf("Received block epoch notification: %v",
				blockHeight)

			m.currentHeight = uint32(blockHeight)
			err := m.handleBlockEpoch(ctxc, m.currentHeight)
			if err != nil {
				return err
			}

		case err := <-blockErrChan:
			log.Errorf("received error from block epoch "+
				"notification: %v", err)

			return err

		case err := <-m.criticalErrChan:
			log.Errorf("stopping asset deposit manager due to "+
				"critical error: %v", err)

			return err

		case <-ctx.Done():
			return nil

		}
	}
}

// scheduleNextCall schedules the next call to the manager's main event loop.
// It returns a function that must be called when the call is finished.
func (m *Manager) scheduleNextCall() (func(), error) {
	select {
	case m.callEnter <- struct{}{}:

	case <-m.quit:
		return func() {}, ErrManagerShuttingDown
	}

	return func() {
		m.callLeave <- struct{}{}
	}, nil
}

// criticalError is used to signal that a critical error has occurred. Such
// error will cause the manager to stop and return the (first) error to the
// caller of Run(...).
func (m *Manager) criticalError(err error) {
	select {
	case m.criticalErrChan <- err:
	default:
	}
}

// handleBlockEpoch is called when a new block is added to the chain.
func (m *Manager) handleBlockEpoch(ctx context.Context, height uint32) error {
	return nil
}

// GetBestBlock returns the current best block height of the chain.
func (m *Manager) GetBestBlock() (uint32, error) {
	done, err := m.scheduleNextCall()
	if err != nil {
		return 0, err
	}
	defer done()

	return m.currentHeight, nil
}

// SubscribeDepositUpdates registers a subscriber for deposit state updates.
func (m *Manager) SubscribeDepositUpdates(depositID string,
	subscriber DepositUpdateCallback) error {

	done, err := m.scheduleNextCall()
	if err != nil {
		return err
	}
	defer done()

	d, ok := m.deposits[depositID]
	if !ok {
		return fmt.Errorf("deposit %s not found", depositID)
	}

	log.Infof("Registering deposit state update subscriber: %s", d.ID)

	// Note that for simplicity of design we do not check whether a
	// subscriber is already registered for the deposit.
	m.subscribers[d.ID] = append(m.subscribers[d.ID], subscriber)

	// Send the current deposit info to the subscriber right away.
	subscriber(d.DepositInfo.Copy())

	return nil
}

// handleDepositStateUpdate updates the deposit state in the store and notifies
// all subscribers of the deposit state change.
func (m *Manager) handleDepositStateUpdate(ctx context.Context,
	d *Deposit) error {

	log.Infof("Handling deposit state update: %s, state=%v", d.ID, d.State)

	// Store the deposit state update in the database.
	err := m.store.UpdateDeposit(ctx, d)
	if err != nil {
		return err
	}

	// Notify all subscribers of the deposit state update.
	subscribers, ok := m.subscribers[d.ID]
	if !ok {
		log.Debugf("No subscribers for deposit %s", d.ID)
		return nil
	}

	for _, subscriber := range subscribers {
		go subscriber(d.DepositInfo.Copy())
	}

	return nil
}
