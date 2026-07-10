package deposit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
)

const (
	// MinConfs is the legacy minimum confirmation target deposits had to
	// reach before they were considered ready to be used for swaps.
	MinConfs = 6

	// MaxConfs is unset since we don't require a max number of
	// confirmations for deposits.
	MaxConfs = 0

	// DefaultTransitionTimeout is the default timeout for transitions in
	// the deposit state machine.
	DefaultTransitionTimeout = 5 * time.Second

	// PollInterval is the interval in which we poll for new deposits to our
	// static address.
	PollInterval = 10 * time.Second
)

// ManagerConfig holds the configuration for the address manager.
type ManagerConfig struct {
	// AddressManager is the address manager that is used to fetch static
	// address parameters.
	AddressManager AddressManager

	// Store is the database store that is used to store static address
	// related records.
	Store Store

	// WalletKit is the wallet client that is used to derive new keys from
	// lnd's wallet.
	WalletKit lndclient.WalletKitClient

	// ChainNotifier is the chain notifier that is used to listen for new
	// blocks.
	ChainNotifier lndclient.ChainNotifierClient

	// Signer is the signer client that is used to sign transactions.
	Signer lndclient.SignerClient
}

// Manager manages the address state machines.
//
// Lock order: if both Manager.mu and a Deposit lock are needed, acquire
// Manager.mu before Deposit.Lock. Never acquire Manager.mu while holding a
// Deposit lock. Multiple deposits must be locked with lockDeposits, which
// canonicalizes lock order by outpoint.
type Manager struct {
	cfg *ManagerConfig

	// mu guards access to the activeDeposits map.
	mu sync.Mutex

	// reconcileMu serializes deposit reconciliation so new deposits are
	// discovered and retained exactly once per outpoint.
	reconcileMu sync.Mutex

	// activeDeposits contains all the active static address outputs.
	activeDeposits map[wire.OutPoint]*FSM

	// deposits contain all the deposits that have ever been made to the
	// static address. This field is used to store and recover deposits. It
	// also serves as a basis for reconciliation of newly detected deposits
	// by matching them against deposits in this map that were already seen.
	deposits map[wire.OutPoint]*Deposit

	// finalizedDepositChan is a channel that receives deposits that have
	// been finalized. The manager will adjust its internal state and flush
	// finalized deposits from its memory.
	finalizedDepositChan chan wire.OutPoint

	// currentHeight stores the currently best known block height.
	currentHeight atomic.Uint32
}

// NewManager creates a new deposit manager.
func NewManager(cfg *ManagerConfig) *Manager {
	return &Manager{
		cfg:                  cfg,
		activeDeposits:       make(map[wire.OutPoint]*FSM),
		deposits:             make(map[wire.OutPoint]*Deposit),
		finalizedDepositChan: make(chan wire.OutPoint),
	}
}

// Run runs the address manager.
func (m *Manager) Run(ctx context.Context, initChan chan struct{}) error {
	newBlockChan, newBlockErrChan, err := m.cfg.ChainNotifier.RegisterBlockEpochNtfn(ctx) //nolint:lll
	if err != nil {
		log.Errorf("unable to register block epoch notifier: %v", err)

		return err
	}

	var startupHeight uint32
	select {
	case height := <-newBlockChan:
		startupHeight = uint32(height)
		m.currentHeight.Store(startupHeight)

	case err = <-newBlockErrChan:
		return err

	case <-ctx.Done():
		return ctx.Err()
	}

	// Recover previous deposits and static address parameters from the DB.
	err = m.recoverDeposits(ctx)
	if err != nil {
		log.Errorf("unable to recover deposits: %v", err)

		return err
	}

	// Reconcile immediately on startup so deposits are available
	// before the first ticker fires.
	err = m.reconcileDeposits(ctx)
	if err != nil {
		log.Errorf("unable to reconcile deposits: %v", err)
	} else {
		// The startup height was consumed before recovered deposit FSMs
		// existed. Replay it so already-expired recovered deposits can act
		// immediately, but only after their wallet view is fresh.
		err = m.notifyActiveDeposits(ctx, startupHeight)
		if err != nil {
			return err
		}
	}

	// Start the deposit notifier.
	m.pollDeposits(ctx)

	// Communicate to the caller that the address manager has completed its
	// initialization.
	close(initChan)

	for {
		select {
		case height := <-newBlockChan:
			m.currentHeight.Store(uint32(height))

			err := m.reconcileDeposits(ctx)
			if err != nil {
				log.Errorf("unable to reconcile deposits: %v", err)
				continue
			}

			err = m.notifyActiveDeposits(ctx, uint32(height))
			if err != nil {
				return err
			}

		case outpoint := <-m.finalizedDepositChan:
			// If deposits notify us about their finalization, flush
			// the finalized deposit from memory.
			m.removeActiveDeposit(outpoint)

		case err = <-newBlockErrChan:
			return err

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// notifyActiveDeposits informs all active deposit FSMs about a new block
// height.
func (m *Manager) notifyActiveDeposits(ctx context.Context,
	height uint32) error {

	m.mu.Lock()
	activeDeposits := make([]*FSM, 0, len(m.activeDeposits))
	for _, fsm := range m.activeDeposits {
		activeDeposits = append(activeDeposits, fsm)
	}
	m.mu.Unlock()

	for _, fsm := range activeDeposits {
		select {
		case fsm.blockNtfnChan <- height:

		case <-fsm.quitChan:
			continue

		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
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
		// TODO(hieblmi): Add a unit test that would fail with the
		//  dead-lock before the fsm was passed in.
		go func(fsm *FSM) {
			err := fsm.SendEvent(ctx, OnRecover, nil)
			if err != nil {
				log.Errorf("Error sending OnRecover event: %v",
					err)
			}
		}(fsm)

		m.mu.Lock()
		m.activeDeposits[d.OutPoint] = fsm
		m.mu.Unlock()
	}

	return nil
}

// pollDeposits periodically polls for new deposits to our static address. This
// complements the block-driven reconciliation in the main event loop: while new
// blocks trigger reconcileDeposits to promptly detect confirmations, the ticker
// here catches deposits that appear in the mempool between blocks.
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

// EnsureDepositsFresh reconciles the cached active deposit set with lnd's
// current wallet view. Spending paths call this before selecting deposits so
// stale persisted records are not treated as live funds. This can happen when
// an unconfirmed funding transaction is replaced, a confirmed deposit is
// reorged out, or the output was spent outside the active manager path.
func (m *Manager) EnsureDepositsFresh(ctx context.Context) error {
	return m.reconcileDeposits(ctx)
}

// reconcileDeposits fetches all spends to our static addresses from our lnd
// wallet and matches it against the deposits in our memory that we've seen so
// far. It picks the newly identified deposits and starts a state machine per
// deposit to track its progress.
func (m *Manager) reconcileDeposits(ctx context.Context) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	log.Tracef("Reconciling new deposits...")

	utxos, err := m.cfg.AddressManager.ListUnspent(
		ctx, 0, MaxConfs,
	)
	if err != nil {
		return fmt.Errorf("unable to list new deposits: %w", err)
	}

	currentHeight := m.currentHeight.Load()
	err = m.updateDepositConfirmations(ctx, utxos, currentHeight)
	if err != nil {
		return fmt.Errorf("unable to update deposit "+
			"confirmations: %w", err)
	}

	err = m.syncActiveDeposits(ctx, utxos)
	if err != nil {
		return fmt.Errorf("unable to sync active deposits: %w", err)
	}

	newDeposits := m.filterNewDeposits(utxos)
	if len(newDeposits) == 0 {
		log.Tracef("No new deposits...")
		return nil
	}

	for _, utxo := range newDeposits {
		deposit, err := m.createNewDeposit(ctx, utxo, currentHeight)
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
	utxo *lnwallet.Utxo, currentHeight uint32) (*Deposit, error) {

	confirmationHeight, err := confirmationHeightForUtxo(
		currentHeight, utxo,
	)
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

	addressParams := m.cfg.AddressManager.GetParameters(utxo.PkScript)
	if addressParams == nil {
		return nil, fmt.Errorf("missing static address parameters "+
			"for deposit %v", utxo.OutPoint)
	}
	if addressParams.ID <= 0 {
		return nil, fmt.Errorf("missing static address ID for deposit %v",
			utxo.OutPoint)
	}

	deposit := &Deposit{
		ID:                   id,
		state:                Deposited,
		OutPoint:             utxo.OutPoint,
		Value:                utxo.Value,
		ConfirmationHeight:   confirmationHeight,
		TimeOutSweepPkScript: timeoutSweepPkScript,
		AddressParams:        addressParams,
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

// confirmationHeightForUtxo derives the first confirmation height of a wallet
// UTXO from the manager's current block height. Unconfirmed UTXOs return 0.
func confirmationHeightForUtxo(currentHeight uint32,
	utxo *lnwallet.Utxo) (int64, error) {

	if utxo.Confirmations <= 0 {
		return 0, nil
	}

	if currentHeight == 0 {
		return 0, errors.New("current block height unavailable")
	}

	firstConfirmationHeight := int64(currentHeight) - utxo.Confirmations + 1
	if firstConfirmationHeight <= 0 {
		return 0, fmt.Errorf("invalid confirmation height %d for %v "+
			"with current height %d and %d confirmations",
			firstConfirmationHeight, utxo.OutPoint, currentHeight,
			utxo.Confirmations)
	}

	return firstConfirmationHeight, nil
}

// updateDepositConfirmations syncs first confirmation heights for deposits that
// are visible in lnd's wallet view.
func (m *Manager) updateDepositConfirmations(ctx context.Context,
	utxos []*lnwallet.Utxo, currentHeight uint32) error {

	for _, utxo := range utxos {
		m.mu.Lock()
		deposit, ok := m.deposits[utxo.OutPoint]
		m.mu.Unlock()
		if !ok {
			continue
		}

		err := func() error {
			deposit.Lock()
			defer deposit.Unlock()

			previousConfirmationHeight := deposit.ConfirmationHeight

			confirmationHeight, err := confirmationHeightForUtxo(
				currentHeight, utxo,
			)
			if err != nil {
				return err
			}

			if deposit.ConfirmationHeight == confirmationHeight {
				return nil
			}

			deposit.ConfirmationHeight = confirmationHeight

			err = m.cfg.Store.UpdateDeposit(ctx, deposit)
			if err != nil {
				deposit.ConfirmationHeight = previousConfirmationHeight

				return err
			}

			return nil
		}()
		if err != nil {
			return err
		}
	}

	return nil
}

// syncActiveDeposits reconciles the live active set with lnd's current wallet
// view. Known Deposited records that are visible but inactive become active
// again, and active Deposited records that are no longer wallet-visible are
// removed from the live set. The DB record is left untouched as historical
// evidence that the outpoint was once detected.
func (m *Manager) syncActiveDeposits(ctx context.Context,
	utxos []*lnwallet.Utxo) error {

	currentUtxos := make(map[wire.OutPoint]struct{}, len(utxos))
	for _, utxo := range utxos {
		currentUtxos[utxo.OutPoint] = struct{}{}
	}

	type deactivatedDeposit struct {
		outpoint wire.OutPoint
		fsm      *FSM
	}

	toActivate := make([]*Deposit, 0, len(utxos))
	var toDeactivate []deactivatedDeposit
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		toDeactivate = make(
			[]deactivatedDeposit, 0, len(m.activeDeposits),
		)

		for _, utxo := range utxos {
			deposit, ok := m.deposits[utxo.OutPoint]
			if !ok {
				continue
			}

			if _, active := m.activeDeposits[utxo.OutPoint]; active {
				continue
			}

			if !deposit.IsInState(Deposited) {
				continue
			}

			toActivate = append(toActivate, deposit)
		}

		for outpoint, fsm := range m.activeDeposits {
			if _, ok := currentUtxos[outpoint]; ok {
				continue
			}

			if fsm == nil || fsm.deposit == nil {
				continue
			}

			if !fsm.deposit.IsInState(Deposited) {
				continue
			}

			delete(m.activeDeposits, outpoint)
			toDeactivate = append(toDeactivate, deactivatedDeposit{
				outpoint: outpoint,
				fsm:      fsm,
			})
		}
	}()

	for _, deactivated := range toDeactivate {
		deactivated.fsm.Stop()

		log.Infof("Removed vanished deposit %v from active set",
			deactivated.outpoint)
	}

	for _, deposit := range toActivate {
		if !deposit.IsInState(Deposited) {
			continue
		}

		err := m.startDepositFsm(ctx, deposit)
		if err != nil {
			m.removeActiveDeposit(deposit.OutPoint)

			return err
		}

		log.Infof("Reactivated visible deposit %v", deposit.OutPoint)
	}

	return nil
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
		err := fsm.SendEvent(ctx, OnStart, nil)
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

	lockedDeposits := lockDeposits(deposits)
	defer unlockDeposits(lockedDeposits)

	filteredDeposits := make([]*Deposit, 0, len(deposits))
	for _, d := range deposits {
		if !d.isInStateNoLock(stateFilter) {
			continue
		}

		filteredDeposits = append(filteredDeposits, d)
	}

	sort.Slice(filteredDeposits, func(i, j int) bool {
		return filteredDeposits[i].GetConfirmationHeightNoLock() <
			filteredDeposits[j].GetConfirmationHeightNoLock()
	})

	return filteredDeposits, nil
}

// AllOutpointsActiveDeposits checks if all deposits referenced by the outpoints
// are in our in-mem active deposits map and in the specified state. If
// fsm.EmptyState is set as targetState all deposits are returned regardless of
// their state. Each existent deposit is locked during the check.
func (m *Manager) AllOutpointsActiveDeposits(outpoints []wire.OutPoint,
	targetState fsm.StateType) ([]*Deposit, bool) {

	if CheckDuplicates(outpoints) != nil {
		return nil, false
	}

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

	lockedDeposits := lockDeposits(deposits)
	defer unlockDeposits(lockedDeposits)
	for _, d := range deposits {
		if !d.isInStateNoLock(targetState) {
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
// heavy things or call external endpoints that can block for a long time as
// this function blocks until the expectedFinalState is reached. The default
// timeout for the transition is set to DefaultTransitionTimeout.
func (m *Manager) TransitionDeposits(ctx context.Context, deposits []*Deposit,
	event fsm.EventType, expectedFinalState fsm.StateType) error {

	outpoints := make([]wire.OutPoint, len(deposits))
	for i, d := range deposits {
		if d == nil {
			return fmt.Errorf("nil deposit at index %d", i)
		}

		outpoints[i] = d.OutPoint
	}
	if err := CheckDuplicates(outpoints); err != nil {
		return fmt.Errorf("duplicate deposit outpoint: %w", err)
	}

	m.mu.Lock()
	stateMachines, _ := m.toActiveDeposits(&outpoints)
	m.mu.Unlock()

	if stateMachines == nil {
		return fmt.Errorf("deposits not found in active deposits")
	}

	lockedDeposits := lockDeposits(deposits)
	defer unlockDeposits(lockedDeposits)
	for _, deposit := range deposits {
		if deposit.isInFinalStateNoLock() {
			return fmt.Errorf("deposit %v is no longer active in "+
				"state %v", deposit.OutPoint,
				deposit.getStateNoLock())
		}
	}

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

// lockDeposits locks deposits in canonical outpoint order and returns the
// ordered slice that must be passed to unlockDeposits.
func lockDeposits(deposits []*Deposit) []*Deposit {
	lockedDeposits := append([]*Deposit(nil), deposits...)
	sort.Slice(lockedDeposits, func(i, j int) bool {
		return lockedDeposits[i].OutPoint.String() <
			lockedDeposits[j].OutPoint.String()
	})

	for _, d := range lockedDeposits {
		d.Lock()
	}

	return lockedDeposits
}

// unlockDeposits unlocks deposits in reverse lock order.
func unlockDeposits(deposits []*Deposit) {
	for i := len(deposits) - 1; i >= 0; i-- {
		d := deposits[i]
		d.Unlock()
	}
}

// removeActiveDeposit removes and stops the FSM for an active outpoint.
func (m *Manager) removeActiveDeposit(outpoint wire.OutPoint) {
	m.mu.Lock()
	fsm, ok := m.activeDeposits[outpoint]
	if ok {
		delete(m.activeDeposits, outpoint)
	}
	m.mu.Unlock()

	if ok {
		fsm.Stop()
	}
}

// GetAllDeposits returns all known deposits from the database.
func (m *Manager) GetAllDeposits(ctx context.Context) ([]*Deposit, error) {
	return m.cfg.Store.AllDeposits(ctx)
}

// GetVisibleDeposits returns deposits that should be exposed through normal
// user-facing views. The database can contain historical Deposited rows whose
// outpoints are no longer present in lnd's current wallet view, for example
// after replacement or reorg. Once the manager has recovered its live cache,
// plain Deposited records are only visible while their outpoint is in the
// active set.
func (m *Manager) GetVisibleDeposits(ctx context.Context) ([]*Deposit, error) {
	deposits, err := m.cfg.Store.AllDeposits(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	liveCacheReady := len(m.deposits) > 0
	filtered := make([]*Deposit, 0, len(deposits))
	for _, d := range deposits {
		if liveCacheReady && d.IsInState(Deposited) {
			if _, ok := m.activeDeposits[d.OutPoint]; !ok {
				continue
			}
		}

		filtered = append(filtered, d)
	}

	return filtered, nil
}

// UpdateDeposit overrides all fields of the deposit with given ID in the store.
func (m *Manager) UpdateDeposit(ctx context.Context, d *Deposit) error {
	d.Lock()
	defer d.Unlock()

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

// DepositsForOutpoints returns all deposits that are behind the given
// outpoints.
func (m *Manager) DepositsForOutpoints(ctx context.Context,
	outpoints []string, ignoreUnknown bool) ([]*Deposit, error) {

	// Check for duplicates.
	existingOutpoints := make(map[string]struct{}, len(outpoints))
	for i, o := range outpoints {
		if _, ok := existingOutpoints[o]; ok {
			return nil, fmt.Errorf("duplicate outpoint %s "+
				"at index %d", o, i)
		}
		existingOutpoints[o] = struct{}{}
	}

	deposits := make([]*Deposit, 0, len(outpoints))
	for _, o := range outpoints {
		op, err := wire.NewOutPointFromString(o)
		if err != nil {
			return nil, err
		}

		deposit, err := m.cfg.Store.DepositForOutpoint(ctx, op.String())
		if err != nil {
			if ignoreUnknown && errors.Is(err, ErrDepositNotFound) {
				continue
			}
			return nil, err
		}

		deposits = append(deposits, deposit)
	}

	return deposits, nil
}
