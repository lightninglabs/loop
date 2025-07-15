package deposit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
)

var (
	// AssetDepositKeyFamily is the key family used for generating asset
	// deposit keys.
	AssetDepositKeyFamily = int32(1122)

	// ErrManagerShuttingDown signals that the asset deposit manager is
	// shutting down and that no further calls should be made to it.
	ErrManagerShuttingDown = errors.New("asset deposit manager is " +
		"shutting down")
)

// DepositUpdateCallback is a callback that is called when a deposit state is
// updated. The callback receives the updated deposit info.
type DepositUpdateCallback func(*Info)

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

	// store is the deposit store.
	store Store

	// sweeper is responsible for assembling and publishing deposit sweeps.
	sweeper *Sweeper

	// currentHeight is the current block height of the chain.
	currentHeight uint32

	// deposits is a map of all active deposits. The key is the deposit ID.
	deposits map[string]*Deposit

	// subscribers is a map of all registered deposit update subscribers.
	// The outer map key is the deposit ID, the inner map key is a unique
	// subscriber ID.
	subscribers map[string]map[uint64]DepositUpdateCallback

	// nextSubscriberID is the next available ID for a subscriber.
	nextSubscriberID uint64

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
	tapClient *assets.TapdClient, store Store,
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
		subscribers:          make(map[string]map[uint64]DepositUpdateCallback), //nolint:lll
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
	subscriber DepositUpdateCallback) (func(), error) {

	done, err := m.scheduleNextCall()
	if err != nil {
		return func() {}, err
	}
	defer done()

	d, ok := m.deposits[depositID]
	if !ok {
		return func() {}, fmt.Errorf("deposit %s not found", depositID)
	}

	log.Infof("Registering deposit state update subscriber for deposit: %s",
		d.ID)

	// Get the next available subscriber ID.
	subscriberID := m.nextSubscriberID
	m.nextSubscriberID++

	// If this is the first subscriber for this deposit, we need to create
	// the inner map.
	if _, ok := m.subscribers[d.ID]; !ok {
		m.subscribers[d.ID] = make(map[uint64]DepositUpdateCallback)
	}

	// Store the subscriber.
	m.subscribers[d.ID][subscriberID] = subscriber

	// Send the current deposit info to the subscriber right away in a
	// non-blocking way.
	go subscriber(d.Info.Copy())

	// Return a function that can be used to unsubscribe.
	unsubscribe := func() {
		done, err := m.scheduleNextCall()
		if err != nil {
			log.Errorf("Error unsubscribing from deposit "+
				"updates: %v", err)
			return
		}
		defer done()

		log.Infof("Unsubscribing from deposit state updates for "+
			"deposit: %s, subscriber: %d", d.ID, subscriberID)

		// If the subscriber map for the deposit exists, delete the
		// subscriber from it.
		if depositSubscribers, ok := m.subscribers[d.ID]; ok {
			delete(depositSubscribers, subscriberID)

			// If there are no more subscribers for this deposit,
			// delete the deposit's entry from the subscribers map.
			if len(depositSubscribers) == 0 {
				delete(m.subscribers, d.ID)
			}
		}
	}

	return unsubscribe, nil
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
		go subscriber(d.Info.Copy())
	}

	return nil
}

// NewDeposit creates a new asset deposit with the given parameters.
func (m *Manager) NewDeposit(ctx context.Context, assetID asset.ID,
	amount uint64, csvExpiry uint32) (Info, error) {

	clientKeyDesc, err := m.walletKit.DeriveNextKey(
		ctx, AssetDepositKeyFamily,
	)
	if err != nil {
		return Info{}, err
	}
	clientInternalPubKey, _, err := DeriveSharedDepositKey(
		ctx, m.signer, clientKeyDesc.PubKey,
	)
	if err != nil {
		return Info{}, err
	}

	clientScriptPubKeyBytes := clientKeyDesc.PubKey.SerializeCompressed()
	clientInternalPubKeyBytes := clientInternalPubKey.SerializeCompressed()

	resp, err := m.depositServiceClient.NewAssetDeposit(
		ctx, &swapserverrpc.NewAssetDepositServerReq{
			AssetId:              assetID[:],
			Amount:               amount,
			ClientInternalPubkey: clientInternalPubKeyBytes,
			ClientScriptPubkey:   clientScriptPubKeyBytes,
			CsvExpiry:            int32(csvExpiry),
		},
	)
	if err != nil {
		log.Errorf("Swap server was unable to create the deposit: %v",
			err)

		return Info{}, err
	}

	serverScriptPubKey, err := btcec.ParsePubKey(resp.ServerScriptPubkey)
	if err != nil {
		return Info{}, err
	}

	serverInternalPubKey, err := btcec.ParsePubKey(
		resp.ServerInternalPubkey,
	)
	if err != nil {
		return Info{}, err
	}

	kit, err := NewKit(
		clientKeyDesc.PubKey, clientInternalPubKey, serverScriptPubKey,
		serverInternalPubKey, clientKeyDesc.KeyLocator, assetID,
		csvExpiry, &m.addressParams,
	)
	if err != nil {
		return Info{}, err
	}

	deposit := &Deposit{
		Kit: kit,
		Info: &Info{
			ID:        resp.DepositId,
			Version:   CurrentProtocolVersion(),
			CreatedAt: time.Now(),
			Amount:    amount,
			Addr:      resp.DepositAddr,
			State:     StateInitiated,
		},
	}

	err = m.store.AddAssetDeposit(ctx, deposit)
	if err != nil {
		log.Errorf("Unable to add deposit to store: %v", err)

		return Info{}, err
	}

	err = m.handleNewDeposit(ctx, deposit)
	if err != nil {
		log.Errorf("Unable to add deposit to active deposits: %v", err)

		return Info{}, err
	}

	return *deposit.Info.Copy(), nil
}

// handleNewDeposit adds the deposit to the active deposits map and starts the
// funding process, all on the main event loop goroutine.
func (m *Manager) handleNewDeposit(ctx context.Context, deposit *Deposit) error {
	done, err := m.scheduleNextCall()
	if err != nil {
		return err
	}
	defer done()

	m.deposits[deposit.ID] = deposit

	return m.fundDepositIfNeeded(ctx, deposit)
}

// fundDepositIfNeeded attempts to fund the passed deposit if it is not already
// funded.
func (m *Manager) fundDepositIfNeeded(ctx context.Context, d *Deposit) error {
	// Now list transfers from tapd and check if the deposit is funded.
	funded, transfer, outIndex, err := m.isDepositFunded(ctx, d)
	if err != nil {
		log.Errorf("Unable to check if deposit %v is funded: %v", d.ID,
			err)

		return err
	}

	if !funded {
		// No funding transfer found, so we'll attempt to fund the
		// deposit by sending the asset to the deposit address. Note
		// that we label the send request with a specific label in order
		// to be able to subscribe to send events with a label filter.
		sendResp, err := m.tapClient.SendAsset(
			ctx, &taprpc.SendAssetRequest{
				TapAddrs: []string{d.Addr},
				Label:    d.fundingLabel(),
			},
		)
		if err != nil {
			log.Errorf("Unable to send asset to deposit %v: %v",
				d.ID, err)

			return err
		}

		// Extract the funding outpoint from the transfer.
		transfer, outIndex, err = d.GetMatchingOut(
			d.Amount, []*taprpc.AssetTransfer{sendResp.Transfer},
		)
		if err != nil {
			log.Errorf("Unable to get funding out for %v: %v ",
				d.ID, err)

			return err
		}
	}

	log.Infof("Deposit %v is funded in anchor %x:%d, "+
		"anchor tx block height: %v", d.ID,
		transfer.AnchorTxHash, outIndex, transfer.AnchorTxBlockHeight)

	// If the deposit is confirmed, then we don't need to wait for the
	// confirmation to happen.
	// TODO(bhandras): once backlog events are supported we can remove this.
	if transfer.AnchorTxBlockHeight != 0 {
		return m.markDepositConfirmed(ctx, d, transfer)
	}

	// Wait for deposit confirmation otherwise.
	err = m.waitForDepositConfirmation(m.runCtx(), d)
	if err != nil {
		log.Errorf("Unable to wait for deposit confirmation: %v", err)

		return err
	}

	return nil
}

// isDepositFunded checks if the deposit is funded with the expected amount. It
// does so by checking if there is a deposit output with the expected keys and
// amount in the list of transfers of the funder.
func (m *Manager) isDepositFunded(ctx context.Context, d *Deposit) (bool,
	*taprpc.AssetTransfer, int, error) {

	res, err := m.tapClient.ListTransfers(
		ctx, &taprpc.ListTransfersRequest{},
	)
	if err != nil {
		return false, nil, 0, err
	}

	transfer, outIndex, err := d.GetMatchingOut(d.Amount, res.Transfers)
	if err != nil {
		return false, nil, 0, err
	}

	if transfer == nil {
		return false, nil, 0, nil
	}

	return true, transfer, outIndex, nil
}

// cacheProofInfo caches the proof information for the deposit in-memory.
func (m *Manager) cacheProofInfo(ctx context.Context, d *Deposit) error {
	proofFile, err := d.ExportProof(ctx, m.tapClient, d.Outpoint)
	if err != nil {
		log.Errorf("Unable to export proof for deposit %v: %v", d.ID,
			err)

		return err
	}

	// Import the proof in order to be able to spend the deposit later on
	// either into an HTLC or a timeout sweep. Note that if the proof is
	// already imoported then this will be a no-op and just return the
	// last proof (ie the deposit proof).
	depositProof, err := m.tapClient.ImportProofFile(
		ctx, proofFile.RawProofFile,
	)
	if err != nil {
		return err
	}

	d.Proof = depositProof

	// Verify that the proof is valid for the deposit and get the root hash
	// which we may use later when signing the HTLC transaction.
	anchorRootHash, err := d.VerifyProof(depositProof)
	if err != nil {
		log.Errorf("failed to verify deposity proof: %v", err)

		return err
	}

	d.AnchorRootHash = anchorRootHash

	return nil
}

// registerDepositAsKnown registers the deposit with the tapd server so it
// can claim the funds committed to the OP_TRUE script.
func (m *Manager) registerDepositAsKnown(ctx context.Context,
	d *Deposit) error {

	opTrueScriptKey, _, _, _, err := htlc.CreateOpTrueLeaf()
	if err != nil {
		return err
	}

	// Declare the script key as known on the underlying tapd. Note that
	// under the hood this is an upsert operation, so if the script is
	// already known it will be a no-op. This is useful since we use the
	// same OP_TRUE script key for all deposits.
	_, err = m.tapClient.DeclareScriptKey(
		ctx, &assetwalletrpc.DeclareScriptKeyRequest{
			ScriptKey: rpcutils.MarshalScriptKey(
				opTrueScriptKey,
			),
		},
	)
	if err != nil {
		return err
	}

	// Now let the underlying tapd know about the deposit transfer so it can
	// then claim the funds committed to the OP_TRUE script.
	opTrueScriptKeyBytes := opTrueScriptKey.PubKey.SerializeCompressed()
	opTrueScriptKeyBytes[0] = secp256k1.PubKeyFormatCompressedEven

	_, err = m.tapClient.RegisterTransfer(
		ctx, &taprpc.RegisterTransferRequest{
			AssetId:   d.AssetID[:],
			ScriptKey: opTrueScriptKeyBytes,
			Outpoint: &taprpc.OutPoint{
				Txid:        d.Outpoint.Hash[:],
				OutputIndex: d.Outpoint.Index,
			},
		},
	)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			log.Debugf("Deposit %v already registered as known: %v",
				d.ID, err)

			return nil
		}

		return err
	}

	return nil
}

// waitForDepositConfirmation waits for the deposit to be confirmed.
func (m *Manager) waitForDepositConfirmation(ctx context.Context,
	d *Deposit) error {

	log.Infof("Subscribing to send events for pending deposit %s, "+
		"addr=%v, created_at=%v", d.ID, d.Addr, d.CreatedAt)

	resChan, errChan, err := m.tapClient.WaitForSendComplete(
		ctx, nil, d.fundingLabel(),
	)
	if err != nil {
		log.Errorf("unable to subscribe to send events: %v", err)
		return err
	}

	go func() {
		select {
		case res := <-resChan:
			done, err := m.scheduleNextCall()
			if err != nil {
				log.Errorf("Unable to schedule next call: %v",
					err)

				m.criticalError(err)
			}
			defer done()

			err = m.markDepositConfirmed(ctx, d, res.Transfer)
			if err != nil {
				log.Errorf("Unable to mark deposit %v as "+
					"confirmed: %v", d.ID, err)

				m.criticalError(err)
			}

		case err := <-errChan:
			m.criticalError(err)
		}
	}()

	return nil
}

// markDepositConfirmed marks the deposit as confirmed in the store and moves it
// to the active deposits map. It also updates the outpoint and the confirmation
// height of the deposit.
func (m *Manager) markDepositConfirmed(ctx context.Context, d *Deposit,
	transfer *taprpc.AssetTransfer) error {

	// Extract the funding outpoint from the transfer.
	_, outIdx, err := d.GetMatchingOut(
		d.Amount, []*taprpc.AssetTransfer{transfer},
	)
	if err != nil {
		return err
	}

	outpoint, err := wire.NewOutPointFromString(
		transfer.Outputs[outIdx].Anchor.Outpoint,
	)
	if err != nil {
		log.Errorf("Unable to parse deposit outpoint %v: %v",
			transfer.Outputs[outIdx].Anchor.Outpoint, err)

		return err
	}

	d.Outpoint = outpoint
	d.PkScript = transfer.Outputs[outIdx].Anchor.PkScript
	d.ConfirmationHeight = transfer.AnchorTxBlockHeight
	d.State = StateConfirmed

	err = m.handleDepositStateUpdate(ctx, d)
	if err != nil {
		return err
	}

	err = m.cacheProofInfo(ctx, d)
	if err != nil {
		log.Errorf("Unable to cache proof info for deposit %v: %v",
			d.ID, err)

		return err
	}

	err = m.registerDepositAsKnown(ctx, d)
	if err != nil {
		log.Errorf("Unable to register deposit %v as known: %v",
			d.ID, err)

		return err
	}

	log.Infof("Deposit %v is confirmed at block %v", d.ID,
		d.ConfirmationHeight)

	return nil
}
