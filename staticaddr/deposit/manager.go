package deposit

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
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
	MinConfs = 6

	// MaxConfs is unset since we don't require a max number of
	// confirmations for deposits.
	MaxConfs = 0

	// DefaultTransitionTimeout is the default timeout for transitions in
	// the deposit state machine.
	DefaultTransitionTimeout = 5 * time.Second
)

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

	// mu guards access to activeDeposits map.
	mu sync.Mutex

	// initChan signals the daemon that the address manager has completed
	// its initialization.
	initChan chan struct{}

	// activeDeposits contains all the active static address outputs.
	activeDeposits map[wire.OutPoint]*FSM

	// initiationHeight stores the currently best known block height.
	initiationHeight uint32

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
	m.initiationHeight = currentHeight

	newBlockChan, newBlockErrChan, err := m.cfg.ChainNotifier.RegisterBlockEpochNtfn(ctx) //nolint:lll
	if err != nil {
		return err
	}

	// Recover previous deposits and static address parameters from the DB.
	err = m.recoverDeposits(ctx)
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
			// Inform all active deposits about a new block arrival.
			for _, fsm := range m.activeDeposits {
				select {
				case fsm.blockNtfnChan <- uint32(height):

				case <-ctx.Done():
					return ctx.Err()
				}
			}
		case outpoint := <-m.finalizedDepositChan:
			// If deposits notify us about their finalization, we
			// update the manager's internal state and flush the
			// finalized deposit from memory.
			delete(m.activeDeposits, outpoint)

		case err = <-newBlockErrChan:
			return err

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// recoverDeposits recovers static address parameters, previous deposits and
// state machines from the database and starts the deposit notifier.
func (m *Manager) recoverDeposits(ctx context.Context) error {
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
		fsm, err := NewFSM(ctx, d, m.cfg, m.finalizedDepositChan, true)
		if err != nil {
			return err
		}

		// Send the OnRecover event to the state machine.
		go func() {
			err = fsm.SendEvent(ctx, OnRecover, nil)
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
		return fmt.Errorf("unable to list new deposits: %w", err)
	}

	newDeposits := m.filterNewDeposits(utxos)
	if err != nil {
		return fmt.Errorf("unable to filter new deposits: %w", err)
	}

	if len(newDeposits) == 0 {
		log.Tracef("No new deposits...")
		return nil
	}

	for _, utxo := range newDeposits {
		deposit, err := m.createNewDeposit(ctx, utxo)
		if err != nil {
			return fmt.Errorf("unable to retain new deposit: %w",
				err)
		}

		log.Debugf("Received deposit: %v", deposit)
		err = m.startDepositFsm(ctx, deposit)
		if err != nil {
			return fmt.Errorf("unable to start new deposit FSM: %w",
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

	m.mu.Lock()
	m.deposits[deposit.OutPoint] = deposit
	m.mu.Unlock()

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
			"deposit, %w", err)
	}

	notifChan, errChan, err :=
		m.cfg.ChainNotifier.RegisterConfirmationsNtfn(
			ctx, &utxo.OutPoint.Hash, addressParams.PkScript,
			MinConfs, addressParams.InitiationHeight,
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
	m.mu.Lock()
	defer m.mu.Unlock()

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
func (m *Manager) startDepositFsm(ctx context.Context, deposit *Deposit) error {
	// Create a state machine for a given deposit.
	fsm, err := NewFSM(ctx, deposit, m.cfg, m.finalizedDepositChan, false)
	if err != nil {
		return err
	}

	// Send the start event to the state machine.
	go func() {
		err = fsm.SendEvent(ctx, OnStart, nil)
		if err != nil {
			log.Errorf("Error sending OnStart event: %v", err)
		}
	}()

	err = fsm.DefaultObserver.WaitForState(ctx, time.Minute, Deposited)
	if err != nil {
		return err
	}

	// Add the FSM to the active FSMs map.
	m.mu.Lock()
	m.activeDeposits[deposit.OutPoint] = fsm
	m.mu.Unlock()

	return nil
}

// GetActiveDepositsInState returns all active deposits. This function is called
// on a client restart before the manager is fully initialized, hence we don't
// have to lock the deposits.
func (m *Manager) GetActiveDepositsInState(stateFilter fsm.StateType) (
	[]*Deposit, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	var deposits []*Deposit
	for _, fsm := range m.activeDeposits {
		deposits = append(deposits, fsm.deposit)
	}

	lockDeposits(deposits)
	defer unlockDeposits(deposits)

	filteredDeposits := make([]*Deposit, 0, len(deposits))
	for _, d := range deposits {
		if !d.IsInStateNoLock(stateFilter) {
			continue
		}

		filteredDeposits = append(filteredDeposits, d)
	}

	sort.Slice(filteredDeposits, func(i, j int) bool {
		return filteredDeposits[i].ConfirmationHeight <
			filteredDeposits[j].ConfirmationHeight
	})

	return filteredDeposits, nil
}

// AllOutpointsActiveDeposits checks if all deposits referenced by the outpoints
// are in our in-mem active deposits map and in the specified state. If
// fsm.EmptyState is set as targetState all deposits are returned regardless of
// their state. Each existent deposit is locked during the check.
func (m *Manager) AllOutpointsActiveDeposits(outpoints []wire.OutPoint,
	targetState fsm.StateType) ([]*Deposit, bool) {

	m.mu.Lock()
	defer m.mu.Unlock()

	_, deposits := m.toActiveDeposits(&outpoints)
	if deposits == nil {
		return nil, false
	}

	// If the targetState is empty we return all active deposits regardless
	// of state.
	if targetState == fsm.EmptyState {
		return deposits, true
	}

	lockDeposits(deposits)
	defer unlockDeposits(deposits)
	for _, d := range deposits {
		if !d.IsInStateNoLock(targetState) {
			return nil, false
		}
	}

	return deposits, true
}

// AllStringOutpointsActiveDeposits converts outpoint strings of format txid:idx
// to wire outpoints and checks if all deposits referenced by the outpoints are
// active and in the specified state. If fsm.EmptyState is referenced as
// stateFilter all deposits are returned regardless of their state.
func (m *Manager) AllStringOutpointsActiveDeposits(outpoints []string,
	stateFilter fsm.StateType) ([]*Deposit, bool) {

	outPoints := make([]wire.OutPoint, len(outpoints))
	for i, o := range outpoints {
		op, err := wire.NewOutPointFromString(o)
		if err != nil {
			return nil, false
		}

		outPoints[i] = *op
	}

	return m.AllOutpointsActiveDeposits(outPoints, stateFilter)
}

// TransitionDeposits allows a caller to transition a set of deposits to a new
// state.
// Caveat: The action triggered by the state transitions should not compute
// heavy things or call external endpoints that can block for a long time.
// Deposits will be released if a transition takes longer than
// DefaultTransitionTimeout which is set to 5 seconds.
func (m *Manager) TransitionDeposits(ctx context.Context, deposits []*Deposit,
	event fsm.EventType, expectedFinalState fsm.StateType) error {

	outpoints := make([]wire.OutPoint, len(deposits))
	for i, d := range deposits {
		outpoints[i] = d.OutPoint
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	stateMachines, _ := m.toActiveDeposits(&outpoints)
	if stateMachines == nil {
		return fmt.Errorf("deposits not found in active deposits")
	}

	lockDeposits(deposits)
	defer unlockDeposits(deposits)
	for _, sm := range stateMachines {
		err := sm.SendEvent(ctx, event, nil)
		if err != nil {
			return err
		}

		err = sm.DefaultObserver.WaitForState(
			ctx, DefaultTransitionTimeout, expectedFinalState,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func lockDeposits(deposits []*Deposit) {
	for _, d := range deposits {
		d.Lock()
	}
}

func unlockDeposits(deposits []*Deposit) {
	for _, d := range deposits {
		d.Unlock()
	}
}

// GetAllDeposits returns all active deposits.
func (m *Manager) GetAllDeposits(ctx context.Context) ([]*Deposit, error) {
	return m.cfg.Store.AllDeposits(ctx)
}

// UpdateDeposit overrides all fields of the deposit with given ID in the store.
func (m *Manager) UpdateDeposit(ctx context.Context, d *Deposit) error {
	return m.cfg.Store.UpdateDeposit(ctx, d)
}

// toActiveDeposits converts a list of outpoints to a list of FSMs and deposits.
// The caller should call mu.Lock() before calling this function.
func (m *Manager) toActiveDeposits(outpoints *[]wire.OutPoint) ([]*FSM,
	[]*Deposit) {

	fsms := make([]*FSM, 0, len(*outpoints))
	deposits := make([]*Deposit, 0, len(*outpoints))
	for _, o := range *outpoints {
		sm, ok := m.activeDeposits[o]
		if !ok {
			return nil, nil
		}

		fsms = append(fsms, sm)
		deposits = append(deposits, m.deposits[o])
	}

	return fsms, deposits
}
