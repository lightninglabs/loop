package deposit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/staticaddr"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	// PollInterval is the interval in which we poll for new deposits to our
	// static address.
	PollInterval = 10 * time.Second

	// MinConfs is the minimum number of confirmations we require for a
	// deposit to be considered available for loop-ins, coop-spends and
	// timeouts.
	MinConfs = 3

	// MaxConfs is unset since we don't require a max number of
	// confirmations for deposits.
	MaxConfs = 0
)

var (
	log btclog.Logger
)

func init() {
	log = staticaddr.GetLogger()
}

// ManagerConfig holds the configuration for the address manager.
type ManagerConfig struct {
	// AddressClient is the client that communicates with the loop server
	// to manage static addresses.
	AddressClient staticaddressrpc.StaticAddressServerClient

	// AddressManager is the address manager that is used to fetch static
	// address parameters.
	AddressManager AddressManager

	// SwapClient provides loop rpc functionality.
	SwapClient *loop.Client

	// Store is the database store that is used to store static address
	// related records.
	Store Store

	// WalletKit is the wallet client that is used to derive new keys from
	// lnd's wallet.
	WalletKit lndclient.WalletKitClient

	// ChainParams is the chain configuration(mainnet, testnet...) this
	// manager uses.
	ChainParams *chaincfg.Params

	// ChainNotifier is the chain notifier that is used to listen for new
	// blocks.
	ChainNotifier lndclient.ChainNotifierClient

	// Signer is the signer client that is used to sign transactions.
	Signer lndclient.SignerClient
}

// Manager manages the address state machines.
type Manager struct {
	cfg *ManagerConfig

	runCtx context.Context

	sync.Mutex

	// initChan signals the daemon that the address manager has completed
	// its initialization.
	initChan chan struct{}

	// activeDeposits contains all the active static address outputs.
	activeDeposits map[wire.OutPoint]*FSM

	// initiationHeight stores the currently best known block height.
	initiationHeight uint32

	// currentHeight stores the currently best known block height.
	currentHeight uint32

	// deposits contains all the deposits that have ever been made to the
	// static address. This field is used to store and recover deposits. It
	// also serves as basis for reconciliation of newly detected deposits by
	// matching them against deposits in this map that were already seen.
	deposits map[wire.OutPoint]*Deposit

	// finalizedDepositChan is a channel that receives deposits that have
	// been finalized. The manager will adjust its internal state and flush
	// finalized deposits from its memory.
	finalizedDepositChan chan wire.OutPoint
}

// NewManager creates a new deposit manager.
func NewManager(cfg *ManagerConfig) *Manager {
	return &Manager{
		cfg:                  cfg,
		initChan:             make(chan struct{}),
		activeDeposits:       make(map[wire.OutPoint]*FSM),
		deposits:             make(map[wire.OutPoint]*Deposit),
		finalizedDepositChan: make(chan wire.OutPoint),
	}
}

// Run runs the address manager.
func (m *Manager) Run(ctx context.Context, currentHeight uint32) error {
	m.runCtx = ctx

	m.Lock()
	m.currentHeight, m.initiationHeight = currentHeight, currentHeight
	m.Unlock()

	newBlockChan, newBlockErrChan, err := m.cfg.ChainNotifier.RegisterBlockEpochNtfn(m.runCtx) //nolint:lll
	if err != nil {
		return err
	}

	// Recover previous deposits and static address parameters from the DB.
	err = m.recover(m.runCtx)
	if err != nil {
		return err
	}

	// Start the deposit notifier.
	m.pollDeposits(ctx)

	// Communicate to the caller that the address manager has completed its
	// initialization.
	close(m.initChan)

	for {
		select {
		case height := <-newBlockChan:
			m.Lock()
			m.currentHeight = uint32(height)
			m.Unlock()

			// Inform all active deposits about a new block arrival.
			for _, fsm := range m.activeDeposits {
				select {
				case fsm.blockNtfnChan <- uint32(height):

				case <-m.runCtx.Done():
					return m.runCtx.Err()
				}
			}
		case outpoint := <-m.finalizedDepositChan:
			// If deposits notify us about their finalization, we
			// update the manager's internal state and flush the
			// finalized deposit from memory.
			m.finalizeDeposit(outpoint)

		case err := <-newBlockErrChan:
			return err

		case <-m.runCtx.Done():
			return m.runCtx.Err()
		}
	}
}

// recover recovers static address parameters, previous deposits and state
// machines from the database and starts the deposit notifier.
func (m *Manager) recover(ctx context.Context) error {
	log.Infof("Recovering static address parameters and deposits...")

	// Recover deposits.
	deposits, err := m.cfg.Store.AllDeposits(ctx)
	if err != nil {
		return err
	}

	for i, d := range deposits {
		m.deposits[d.OutPoint] = deposits[i]

		// If the current deposit is final it wasn't active when we
		// shut down the client last. So we don't need to start a fsm
		// for it.
		if d.IsInFinalState() {
			continue
		}

		log.Debugf("Recovering deposit %x", d.ID)

		// Create a state machine for a given deposit.
		fsm, err := NewFSM(
			m.runCtx, d, m.cfg,
			m.finalizedDepositChan, true,
		)
		if err != nil {
			return err
		}

		// Send the OnRecover event to the state machine.
		go func() {
			err = fsm.SendEvent(OnRecover, nil)
			if err != nil {
				log.Errorf("Error sending OnStart event: %v",
					err)
			}
		}()

		m.activeDeposits[d.OutPoint] = fsm
	}

	return nil
}

// WaitInitComplete waits until the address manager has completed its setup.
func (m *Manager) WaitInitComplete() {
	defer log.Debugf("Static address deposit manager initiation complete.")
	<-m.initChan
}

// pollDeposits polls new deposits to our static address and notifies the
// manager's event loop about them.
func (m *Manager) pollDeposits(ctx context.Context) {
	log.Debugf("Waiting for new static address deposits...")

	go func() {
		ticker := time.NewTicker(PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				err := m.reconcileDeposits(ctx)
				if err != nil {
					log.Errorf("unable to reconcile "+
						"deposits: %v", err)
				}

			case <-ctx.Done():
				return
			}
		}
	}()
}

// reconcileDeposits fetches all spends to our static address from our lnd
// wallet and matches it against the deposits in our memory that we've seen so
// far. It picks the newly identified deposits and starts a state machine per
// deposit to track its progress.
func (m *Manager) reconcileDeposits(ctx context.Context) error {
	log.Tracef("Reconciling new deposits...")

	utxos, err := m.cfg.AddressManager.ListUnspent(
		ctx, MinConfs, MaxConfs,
	)
	if err != nil {
		return fmt.Errorf("unable to list new deposits: %v", err)
	}

	newDeposits := m.filterNewDeposits(utxos)
	if err != nil {
		return fmt.Errorf("unable to filter new deposits: %v", err)
	}

	if len(newDeposits) == 0 {
		log.Tracef("No new deposits...")
		return nil
	}

	for _, utxo := range newDeposits {
		deposit, err := m.createNewDeposit(ctx, utxo)
		if err != nil {
			return fmt.Errorf("unable to retain new deposit: %v",
				err)
		}

		log.Debugf("Received deposit: %v", deposit)
		err = m.startDepositFsm(deposit)
		if err != nil {
			return fmt.Errorf("unable to start new deposit FSM: %v",
				err)
		}
	}

	return nil
}

// createNewDeposit transforms the wallet utxo into a deposit struct and stores
// it in our database and manager memory.
func (m *Manager) createNewDeposit(ctx context.Context,
	utxo *lnwallet.Utxo) (*Deposit, error) {

	blockHeight, err := m.getBlockHeight(ctx, utxo)
	if err != nil {
		return nil, err
	}

	// Get the sweep pk script.
	addr, err := m.cfg.WalletKit.NextAddr(
		ctx, lnwallet.DefaultAccountName,
		walletrpc.AddressType_TAPROOT_PUBKEY, false,
	)
	if err != nil {
		return nil, err
	}

	timeoutSweepPkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, err
	}

	id, err := GetRandomDepositID()
	if err != nil {
		return nil, err
	}
	deposit := &Deposit{
		ID:                   id,
		state:                Deposited,
		OutPoint:             utxo.OutPoint,
		Value:                utxo.Value,
		ConfirmationHeight:   int64(blockHeight),
		TimeOutSweepPkScript: timeoutSweepPkScript,
	}

	err = m.cfg.Store.CreateDeposit(ctx, deposit)
	if err != nil {
		return nil, err
	}

	m.Lock()
	m.deposits[deposit.OutPoint] = deposit
	m.Unlock()

	return deposit, nil
}

// getBlockHeight retrieves the block height of a given utxo.
func (m *Manager) getBlockHeight(ctx context.Context,
	utxo *lnwallet.Utxo) (uint32, error) {

	addressParams, err := m.cfg.AddressManager.GetStaticAddressParameters(
		ctx,
	)
	if err != nil {
		return 0, fmt.Errorf("couldn't get confirmation height for "+
			"deposit, %v", err)
	}

	notifChan, errChan, err := m.cfg.ChainNotifier.RegisterConfirmationsNtfn( //nolint:lll
		ctx, &utxo.OutPoint.Hash, addressParams.PkScript, MinConfs,
		int32(m.initiationHeight),
	)
	if err != nil {
		return 0, err
	}

	select {
	case tx := <-notifChan:
		return tx.BlockHeight, nil

	case err := <-errChan:
		return 0, err

	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// filterNewDeposits filters the given utxos for new deposits that we haven't
// seen before.
func (m *Manager) filterNewDeposits(utxos []*lnwallet.Utxo) []*lnwallet.Utxo {
	m.Lock()
	defer m.Unlock()

	var newDeposits []*lnwallet.Utxo
	for _, utxo := range utxos {
		_, ok := m.deposits[utxo.OutPoint]
		if !ok {
			newDeposits = append(newDeposits, utxo)
		}
	}

	return newDeposits
}

// startDepositFsm creates a new state machine flow from the latest deposit to
// our static address.
func (m *Manager) startDepositFsm(deposit *Deposit) error {
	// Create a state machine for a given deposit.
	fsm, err := NewFSM(
		m.runCtx, deposit, m.cfg, m.finalizedDepositChan, false,
	)
	if err != nil {
		return err
	}

	// Send the start event to the state machine.
	go func() {
		err = fsm.SendEvent(OnStart, nil)
		if err != nil {
			log.Errorf("Error sending OnStart event: %v", err)
		}
	}()

	err = fsm.DefaultObserver.WaitForState(m.runCtx, time.Minute, Deposited)
	if err != nil {
		return err
	}

	// Add the FSM to the active FSMs map.
	m.Lock()
	m.activeDeposits[deposit.OutPoint] = fsm
	m.Unlock()

	return nil
}

func (m *Manager) finalizeDeposit(outpoint wire.OutPoint) {
	m.Lock()
	delete(m.activeDeposits, outpoint)
	delete(m.deposits, outpoint)
	m.Unlock()
}
