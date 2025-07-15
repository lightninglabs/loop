package deposit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
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
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/protobuf/proto"
)

var (
	// AssetDepositKeyFamily is the key family used for generating asset
	// deposit keys.
	AssetDepositKeyFamily = int32(1122)

	// ErrManagerShuttingDown signals that the asset deposit manager is
	// shutting down and that no further calls should be made to it.
	ErrManagerShuttingDown = errors.New("asset deposit manager is " +
		"shutting down")

	// lockExpiration us the expiration time we use for sweep fee
	// paying inputs.
	lockExpiration = time.Hour * 24
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

	// pendingSweeps is a map of all pending timeout sweeps. The key is the
	// deposit ID.
	pendingSweeps map[string]struct{}

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
		pendingSweeps:        make(map[string]struct{}),
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

	err := m.recoverDeposits(ctx)
	if err != nil {
		log.Errorf("Unable to recover deposits: %v", err)

		return err
	}

	blockChan, blockErrChan, err := m.chainNotifier.RegisterBlockEpochNtfn(
		ctxc,
	)
	if err != nil {
		log.Errorf("unable to register for block epoch notifications: "+
			"%v", err)

		return err
	}

	// Wake the manager up very 10 seconds to check if there're any pending
	// chores to do.
	const wakeupInterval = time.Duration(10) * time.Second
	withdrawTicker := time.NewTicker(wakeupInterval)

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

		case <-withdrawTicker.C:
			err := m.publishPendingWithdrawals(ctx)
			if err != nil {
				log.Errorf("Unable to publish pending "+
					"withdrawals: %v", err)

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

// recoverDeposits recovers all active deppsits when the deposit manager starts.
func (m *Manager) recoverDeposits(ctx context.Context) error {
	// Fetch all active deposits from the store to kick-off the manager.
	activeDeposits, err := m.store.GetActiveDeposits(ctx)
	if err != nil {
		log.Errorf("Unable to fetch deposits from store: %v", err)

		return err
	}

	for i := range activeDeposits {
		d := &activeDeposits[i]
		log.Infof("Recovering deposit %v (state=%s)", d.ID, d.State)

		m.deposits[d.ID] = d
		_, _, _, err = m.isDepositFunded(ctx, d)
		if err != nil {
			return err
		}

		if d.State == StateInitiated {
			// If the deposit has just been initiated, then we need
			// to ensure that it is funded.
			err = m.fundDepositIfNeeded(ctx, d)
			if err != nil {
				log.Errorf("Unable to fund deposit %v: %v",
					d.ID, err)

				return err
			}
		} else {
			// Cache proof info of the deposit in-memory.
			err = m.cacheProofInfo(ctx, d)
			if err != nil {
				return err
			}

			// Register the deposit as known so that we can claim
			// the funds committed to the OP_TRUE script. If the
			// deposit is already registered, this will be a no-op.
			err = m.registerDepositAsKnown(ctx, d)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// handleBlockEpoch is called when a new block is added to the chain.
func (m *Manager) handleBlockEpoch(ctx context.Context, height uint32) error {
	for _, d := range m.deposits {
		if d.State != StateConfirmed {
			continue
		}

		log.Debugf("Checking if deposit %v is expired, expiry=%v", d.ID,
			d.ConfirmationHeight+d.CsvExpiry)

		if height < d.ConfirmationHeight+d.CsvExpiry {
			continue
		}

		err := m.handleDepositExpired(ctx, d)
		if err != nil {
			log.Errorf("Unable to update deposit %v state: %v",
				d.ID, err)

			return err
		}
	}

	// Now publish the timeout sweeps for all expired deposits and also
	// move them to the pending sweeps map.
	for _, d := range m.deposits {
		// TODO(bhandras): republish will insert a new transfer entry in
		// tapd, despite the transfer already existing. To avoid that,
		// we won't re-publish the timeout sweep for now.
		if d.State != StateExpired {
			continue
		}

		err := m.publishTimeoutSweep(ctx, d)
		if err != nil {
			return err
		}
	}

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

// NewDeposit creates a new asset deposit with the given parameters.
func (m *Manager) NewDeposit(ctx context.Context, assetID asset.ID,
	amount uint64, csvExpiry uint32) (DepositInfo, error) {

	clientKeyDesc, err := m.walletKit.DeriveNextKey(
		ctx, AssetDepositKeyFamily,
	)
	if err != nil {
		return DepositInfo{}, err
	}
	clientInternalPubKey, _, err := DeriveSharedDepositKey(
		ctx, m.signer, clientKeyDesc.PubKey,
	)
	if err != nil {
		return DepositInfo{}, err
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

		return DepositInfo{}, err
	}

	serverScriptPubKey, err := btcec.ParsePubKey(resp.ServerScriptPubkey)
	if err != nil {
		return DepositInfo{}, err
	}

	serverInternalPubKey, err := btcec.ParsePubKey(
		resp.ServerInternalPubkey,
	)
	if err != nil {
		return DepositInfo{}, err
	}

	kit, err := NewKit(
		clientKeyDesc.PubKey, clientInternalPubKey, serverScriptPubKey,
		serverInternalPubKey, clientKeyDesc.KeyLocator, assetID,
		csvExpiry, &m.addressParams,
	)
	if err != nil {
		return DepositInfo{}, err
	}

	deposit := &Deposit{
		Kit: kit,
		DepositInfo: &DepositInfo{
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

		return DepositInfo{}, err
	}

	err = m.handleNewDeposit(ctx, deposit)
	if err != nil {
		log.Errorf("Unable to add deposit to active deposits: %v", err)

		return DepositInfo{}, err
	}

	return *deposit.DepositInfo.Copy(), nil
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

// ListDeposits returns all deposits that are in the given range of
// confirmations.
func (m *Manager) ListDeposits(ctx context.Context, minConfs, maxConfs uint32) (
	[]Deposit, error) {

	bestBlock, err := m.GetBestBlock()
	if err != nil {
		return nil, err
	}

	deposits, err := m.store.GetAllDeposits(ctx)
	if err != nil {
		return nil, err
	}

	// Only filter based on confirmations if the user has set a min or max
	// confs.
	filterConfs := minConfs != 0 || maxConfs != 0

	// Prefilter deposits based on the min/max confs.
	filteredDeposits := make([]Deposit, 0, len(deposits))
	for _, deposit := range deposits {
		if filterConfs {
			// Check that the deposit suits our min/max confs
			// criteria.
			confs := bestBlock - deposit.ConfirmationHeight
			if confs < minConfs || confs > maxConfs {
				continue
			}
		}

		filteredDeposits = append(filteredDeposits, deposit)
	}

	return filteredDeposits, nil
}

// handleDepositStateUpdate updates the deposit state in the store and
// notifies all subscribers of the deposit state change.
func (m *Manager) handleDepositExpired(ctx context.Context, d *Deposit) error {
	d.State = StateExpired
	err := d.GenerateSweepKeys(ctx, m.tapClient)
	if err != nil {
		log.Errorf("Unable to generate sweep keys for deposit %v: %v",
			d.ID, err)
	}

	return m.handleDepositStateUpdate(ctx, d)
}

// publishTimeoutSweep publishes a timeout sweep for the deposit. As we use the
// same lock ID for the sponsoring inputs, it's possible to republish the sweep
// however it'll create a new transfer entry in tapd, which we want to avoid
// (for now).
func (m *Manager) publishTimeoutSweep(ctx context.Context, d *Deposit) error {
	log.Infof("(Re)publishing timeout sweep for deposit %v", d.ID)

	// TODO(bhandras): conf target should be dynamic/configrable.
	const confTarget = 2
	feeRateSatPerKw, err := m.walletKit.EstimateFeeRate(
		ctx, confTarget,
	)
	if err != nil {
		return err
	}

	lockID, err := d.lockID()
	if err != nil {
		return err
	}

	snedResp, err := m.sweeper.PublishDepositTimeoutSweep(
		ctx, d.Kit, d.Proof, asset.NewScriptKey(d.SweepScriptKey),
		d.SweepInternalKey, d.timeoutSweepLabel(),
		feeRateSatPerKw.FeePerVByte(), lockID, lockExpiration,
	)
	if err != nil {
		// TODO(bhandras): handle republish errors.
		log.Infof("Unable to publish timeout sweep for deposit %v: %v",
			d.ID, err)
	} else {
		log.Infof("Published timeout sweep for deposit %v: %x", d.ID,
			snedResp.Transfer.AnchorTxHash)

		// Update deposit state on first successful publish.
		if d.State != StateTimeoutSweepPublished {
			d.State = StateTimeoutSweepPublished
			err = m.handleDepositStateUpdate(ctx, d)
			if err != nil {
				log.Errorf("Unable to update deposit %v "+
					"state: %v", d.ID, err)

				return err
			}
		}
	}

	// Start monitoring the sweep unless we're already doing so.
	if _, ok := m.pendingSweeps[d.ID]; !ok {
		err := m.waitForDepositSweep(ctx, d, d.timeoutSweepLabel())
		if err != nil {
			log.Errorf("Unable to wait for deposit %v spend: %v",
				d.ID, err)

			return err
		}

		m.pendingSweeps[d.ID] = struct{}{}
	}

	return nil
}

// waitForDepositSpend waits for the deposit to be spent. It subscribes to
// receive events for the deposit's sweep address notifying us once the transfer
// has completed.
func (m *Manager) waitForDepositSweep(ctx context.Context, d *Deposit,
	label string) error {

	log.Infof("Waiting for deposit sweep confirmation %s", d.ID)

	eventChan, errChan, err := m.tapClient.WaitForSendComplete(
		ctx, d.SweepScriptKey.SerializeCompressed(), label,
	)
	if err != nil {
		log.Errorf("unable to subscribe to send events for deposit "+
			"sweep: %v", err,
		)
	}

	go func() {
		select {
		case event := <-eventChan:
			// At this point we can consider the deposit confirmed.
			err = m.handleDepositSpend(ctx, d, event.Transfer)
			if err != nil {
				m.criticalError(err)
			}

		case err := <-errChan:
			m.criticalError(err)
		}
	}()

	return nil
}

func formatProtoJSON(resp proto.Message) (string, error) {
	jsonBytes, err := taprpc.ProtoJSONMarshalOpts.Marshal(resp)
	if err != nil {
		return "", err
	}

	return string(jsonBytes), nil
}

func toJSON(resp proto.Message) string {
	jsonStr, _ := formatProtoJSON(resp)

	return jsonStr
}

// handleDepositSpend is called when the deposit is spent. It updates the
// deposit state and releases the inputs used for the deposit sweep.
func (m *Manager) handleDepositSpend(ctx context.Context, d *Deposit,
	transfer *taprpc.AssetTransfer) error {

	done, err := m.scheduleNextCall()
	if err != nil {
		log.Errorf("Unable to schedule next call: %v", err)

		return err
	}
	defer done()

	switch d.State {
	case StateTimeoutSweepPublished:
		fallthrough
	case StateCooperativeSweepPublished:
		d.State = StateSwept

		err := m.releaseDepositSweepInputs(ctx, d)
		if err != nil {
			log.Errorf("Unable to release deposit sweep inputs: "+
				"%v", err)

			return err
		}

	default:
		err := fmt.Errorf("Spent deposit %s in unexpected state %s",
			d.ID, d.State)

		log.Errorf(err.Error())

		return err
	}

	log.Tracef("Deposit %s spent in transfer: %s\n", d.ID, toJSON(transfer))

	// TODO(bhandras): should save the spend details to the store?
	err = m.handleDepositStateUpdate(ctx, d)
	if err != nil {
		return err
	}

	// Sanity check that the deposit is in the pending sweeps map.
	if _, ok := m.pendingSweeps[d.ID]; !ok {
		log.Errorf("Deposit %v not found in pending deposits", d.ID)
	}

	// We can now remove the deposit from the pending sweeps map as we don't
	// need to monitor for the spend anymore.
	delete(m.pendingSweeps, d.ID)

	return nil
}

// releaseDepositSweepInputs releases the inputs that were used for the deposit
// sweep.
func (m *Manager) releaseDepositSweepInputs(ctx context.Context,
	d *Deposit) error {

	lockID, err := d.lockID()
	if err != nil {
		return err
	}

	leases, err := m.walletKit.ListLeases(ctx)
	if err != nil {
		return err
	}

	for _, lease := range leases {
		if lease.LockID != lockID {
			continue
		}

		// Unlock any UTXOs that were used for the deposit sweep.
		err = m.walletKit.ReleaseOutput(ctx, lockID, lease.Outpoint)
		if err != nil {
			return err
		}
	}

	return nil
}

// WithdrawDeposits withdraws the deposits with the given IDs. It will first ask
// the server for the deposit keys, then initate the withdrawal by updating the
// deposit state.
func (m *Manager) WithdrawDeposits(ctx context.Context,
	depositIDs []string) error {

	done, err := m.scheduleNextCall()
	if err != nil {
		return err
	}
	defer done()

	for _, depositID := range depositIDs {
		d, ok := m.deposits[depositID]
		if !ok {
			return fmt.Errorf("deposit %v not found", depositID)
		}

		if d.State != StateConfirmed {
			return fmt.Errorf("deposit %v is not withdrawable, "+
				"current state: %v", depositID, d.State)
		}

		log.Infof("Initiating deposit withdrawal %v: %v",
			depositID, d.Amount)
	}

	keys, err := m.depositServiceClient.WithdrawAssetDeposits(
		ctx, &swapserverrpc.WithdrawAssetDepositsServerReq{
			DepositIds: depositIDs,
		},
	)
	if err != nil {
		return fmt.Errorf("unable to request withdrawal: %w", err)
	}

	for depositID, privKeyBytes := range keys.DepositKeys {
		d, ok := m.deposits[depositID]
		if !ok {
			log.Warnf("Skipping withdrawal of unknown deposit: %v",
				depositID)
			continue
		}

		privKey, pubKey := btcec.PrivKeyFromBytes(privKeyBytes)
		if !d.CoSignerInternalKey.IsEqual(pubKey) {
			return fmt.Errorf("revealed co-signer internal key "+
				"does not match local key for %v", depositID)
		}

		err := m.store.SetAssetDepositServerKey(ctx, depositID, privKey)
		if err != nil {
			return err
		}

		d.State = StateWithdrawn
		err = d.GenerateSweepKeys(ctx, m.tapClient)
		if err != nil {
			log.Errorf("Unable to generate sweep keys for deposit "+
				"withdrawal %v: %v", d.ID, err)

			return err
		}

		err = m.handleDepositStateUpdate(ctx, d)
		if err != nil {
			return err
		}
	}

	return nil
}

// publishPendingWithdrawals publishes any pending deposit withdrawals.
func (m *Manager) publishPendingWithdrawals(ctx context.Context) error {
	for _, d := range m.deposits {
		// TODO(bhandras): republish on StateCooperativeSweepPublished.
		if d.State != StateWithdrawn {
			continue
		}

		serverKey, err := m.store.GetAssetDepositServerKey(
			ctx, d.ID,
		)
		if err != nil {
			return err
		}

		lockID, err := d.lockID()
		if err != nil {
			return err
		}

		// TODO(bhandras): conf target should be dynamic/configrable.
		const confTarget = 2
		feeRateSatPerKw, err := m.walletKit.EstimateFeeRate(
			ctx, confTarget,
		)
		if err != nil {
			return err
		}

		funder := true
		sendResp, err := m.sweeper.PublishDepositSweepMuSig2(
			ctx, d.Kit, funder, d.Proof, serverKey,
			asset.NewScriptKey(d.SweepScriptKey),
			d.SweepInternalKey, d.withdrawLabel(),
			feeRateSatPerKw.FeePerVByte(), lockID, lockExpiration,
		)
		if err != nil {
			log.Errorf("Unable to publish deposit sweep for %v: %v",
				d.ID, err)
		} else {
			log.Infof("Published sweep for deposit %v: %v", d.ID,
				sendResp.Transfer.AnchorTxHash)

			d.State = StateCooperativeSweepPublished
			err = m.handleDepositStateUpdate(ctx, d)
			if err != nil {
				log.Errorf("Unable to update deposit %v "+
					"state: %v", d.ID, err)

				return err
			}
		}

		// Start monitoring the sweep unless we're already doing so.
		if _, ok := m.pendingSweeps[d.ID]; !ok {
			err := m.waitForDepositSweep(ctx, d, d.withdrawLabel())
			if err != nil {
				log.Errorf("Unable to wait for deposit %v "+
					"spend: %v", d.ID, err)

				return err
			}

			m.pendingSweeps[d.ID] = struct{}{}
		}
	}

	return nil
}

// RevealDepositKeys reveals the internal keys for the given deposit IDs to
// the swap server.
func (m *Manager) RevealDepositKeys(ctx context.Context,
	depositIDs []string) error {

	done, err := m.scheduleNextCall()
	if err != nil {
		return err
	}
	defer done()

	// First check that all requested deposits are in the required state and
	// collect the keys.
	keys := make(map[string][]byte, len(depositIDs))
	for _, depositID := range depositIDs {
		d, ok := m.deposits[depositID]
		if !ok {
			log.Warnf("Can't reveal key for deposit %v as it is "+
				"not active", depositID)
		}

		if d.State != StateConfirmed && d.State != StateKeyRevealed {
			return fmt.Errorf("deposit %v key cannot be revealed",
				depositID)
		}

		internalPubKey, internalPrivKey, err := DeriveSharedDepositKey(
			ctx, m.signer, d.FunderScriptKey,
		)
		if err != nil {
			return err
		}

		if !d.FunderInternalKey.IsEqual(internalPubKey) {
			log.Errorf("Funder internal key %x does not match "+
				"expected %x for deposit %v",
				d.FunderInternalKey.SerializeCompressed(),
				internalPubKey.SerializeCompressed(), depositID)

			return fmt.Errorf("funder internal key mismatch")
		}

		keys[depositID] = internalPrivKey.Serialize()
	}

	// Update the deposit state before we actually push the keys to the
	// server. Otherwise we may fail to update the state in our database,
	// despite the server accepting the keys.
	for depositID := range keys {
		d := m.deposits[depositID]
		d.State = StateKeyRevealed
		err = m.handleDepositStateUpdate(ctx, d)
		if err != nil {
			return err
		}

		log.Infof("Revealing deposit key for %v: pub=%x", depositID,
			d.FunderInternalKey.SerializeCompressed())
	}

	// Now push the keys to the server.
	_, err = m.depositServiceClient.PushAssetDepositKeys(
		ctx, &swapserverrpc.PushAssetDepositKeysReq{
			DepositKeys: keys,
		},
	)
	if err != nil {
		return err
	}

	return err
}

// PushHtlcSig will partially a deposit spending zero-fee HTLC and send the
// resulting signature to the swap server.
func (m *Manager) PushHtlcSig(ctx context.Context, depositID string,
	serverNonce [musig2.PubNonceSize]byte, hash lntypes.Hash,
	csvExpiry uint32) error {

	done, err := m.scheduleNextCall()
	if err != nil {
		return err
	}
	defer done()

	deposit, ok := m.deposits[depositID]
	if !ok {
		return fmt.Errorf("deposit %v not available", depositID)
	}

	_, htlcPkt, _, _, _, err := m.sweeper.GetHTLC(
		ctx, deposit.Kit, deposit.Proof, deposit.Amount, hash,
		csvExpiry,
	)
	if err != nil {
		log.Errorf("Unable to get HTLC packet: %v", err)

		return err
	}

	prevOutFetcher := wallet.PsbtPrevOutputFetcher(htlcPkt)
	sigHash, err := getSigHash(htlcPkt.UnsignedTx, 0, prevOutFetcher)
	if err != nil {
		return err
	}

	funder := true
	depositSigner := NewMuSig2Signer(
		m.signer, deposit.Kit, funder, deposit.AnchorRootHash,
	)

	err = depositSigner.NewSession(ctx)
	if err != nil {
		return fmt.Errorf("Unable to create MuSig2 session: %w", err)
	}

	publicNonce, err := depositSigner.PubNonce()
	if err != nil {
		return err
	}

	partialSig, err := depositSigner.PartialSignMuSig2(
		serverNonce, sigHash,
	)
	if err != nil {
		log.Errorf("Unable to create partial deposit signature %v: %v",
			deposit.ID, err)

		return err
	}

	var pktBuf bytes.Buffer
	err = htlcPkt.Serialize(&pktBuf)
	if err != nil {
		return err
	}

	// TODO(bhandras): the server should return the final signature.
	_, err = m.depositServiceClient.PushAssetDepositHtlcSigs(
		ctx, &swapserverrpc.PushAssetDepositHtlcSigsReq{
			Hash:      hash[:],
			CsvExpiry: csvExpiry,
			PartialSigs: []*swapserverrpc.AssetDepositPartialSig{
				{
					DepositId:  depositID,
					Nonce:      publicNonce[:],
					PartialSig: partialSig,
				},
			},
			HtlcPsbt: pktBuf.Bytes(),
		},
	)

	return err
}
