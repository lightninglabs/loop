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
	"github.com/lightninglabs/loop/staticaddr/address"
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

	// vanishedDepositThreshold is the number of consecutive wallet
	// observations in which a Deposited outpoint must be missing before we
	// mark it replaced.
	//
	// A single miss can happen during a transient wallet-view gap while lnd is
	// processing a replacement or reorg. Requiring two misses keeps that narrow
	// race recoverable without leaving vanished deposits selectable forever. At
	// the default PollInterval, this means a vanished deposit can remain active
	// for up to roughly 20 seconds.
	vanishedDepositThreshold = 2
)

// ManagerConfig holds the configuration for the address manager.
type ManagerConfig struct {
	// AddressManager is the address manager that is used to fetch static
	// address parameters.
	AddressManager AddressManager

	// ChainKit is used to query the best known chain tip when deriving
	// confirmation heights from wallet UTXOs.
	ChainKit lndclient.ChainKitClient

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
// Deposit lock.
type Manager struct {
	cfg *ManagerConfig

	// mu guards access to the activeDeposits map.
	mu sync.Mutex

	// reconcileMu serializes deposit recovery and reconciliation so restore
	// requests can't race the background polling loop, and new deposits are
	// discovered and retained exactly once per outpoint.
	reconcileMu sync.Mutex

	// activeDeposits contains all the active static address outputs.
	activeDeposits map[wire.OutPoint]*FSM

	// missingDeposits counts consecutive wallet observations in which a
	// Deposited outpoint was missing from the wallet view.
	missingDeposits map[wire.OutPoint]uint8

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
		missingDeposits:      make(map[wire.OutPoint]uint8),
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
	_, err = m.ReconcileDeposits(ctx)
	if err != nil {
		log.Errorf("unable to reconcile deposits: %v", err)
	}

	// The startup height was consumed before recovered deposit FSMs existed.
	// Replay it so already-expired recovered deposits can act immediately.
	err = m.notifyActiveDeposits(ctx, startupHeight)
	if err != nil {
		return err
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

			_, err := m.ReconcileDeposits(ctx)
			if err != nil {
				log.Errorf("unable to reconcile deposits: %v", err)
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
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	log.Infof("Recovering static address parameters and deposits...")

	// Recover deposits.
	deposits, err := m.cfg.Store.AllDeposits(ctx)
	if err != nil {
		return err
	}

	for i, d := range deposits {
		err = m.hydrateLegacyDepositAddressParams(ctx, d)
		if err != nil {
			return err
		}

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

// hydrateLegacyDepositAddressParams fills in address parameters for deposits
// that predate the durable deposit-to-static-address link. Those deposits all
// belonged to the legacy/root static address, so the legacy address manager
// lookup preserves the behavior that existed before multi-address support.
func (m *Manager) hydrateLegacyDepositAddressParams(ctx context.Context,
	deposits ...*Deposit) error {

	needsHydration := false
	for _, d := range deposits {
		if d != nil && d.AddressParams == nil {
			needsHydration = true
			break
		}
	}
	if !needsHydration {
		return nil
	}

	if m.cfg == nil || m.cfg.AddressManager == nil {
		return nil
	}

	var legacyParams *address.Parameters
	for _, d := range deposits {
		if d == nil || d.AddressParams != nil {
			continue
		}

		if legacyParams == nil {
			params, err := m.cfg.AddressManager.
				GetStaticAddressParameters(ctx)
			if err != nil {
				return fmt.Errorf("unable to recover legacy "+
					"static address parameters for deposit %v: %w",
					d.OutPoint, err)
			}
			if params == nil {
				return fmt.Errorf("missing legacy static address "+
					"parameters for deposit %v", d.OutPoint)
			}

			if params.ID <= 0 {
				params.ID, err = m.cfg.AddressManager.
					GetStaticAddressID(ctx, params.PkScript)
				if err != nil {
					return fmt.Errorf("unable to recover legacy "+
						"static address ID for deposit %v: %w",
						d.OutPoint, err)
				}
			}

			legacyParams = params
		}

		d.AddressParams = legacyParams
		d.AddressID = legacyParams.ID
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
				_, err := m.ReconcileDeposits(ctx)
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

// reconcileDeposits fetches all spends to our static addresses from our lnd
// wallet and matches it against the deposits in our memory that we've seen so
// far. It picks the newly identified deposits and starts a state machine per
// deposit to track its progress.
func (m *Manager) reconcileDeposits(ctx context.Context) (int, error) {
	log.Tracef("Reconciling new deposits...")

	utxos, bestHeight, err := m.listUnspentWithBestHeight(ctx)
	if err != nil {
		return 0, err
	}

	err = m.updateDepositConfirmations(ctx, utxos, bestHeight)
	if err != nil {
		return 0, fmt.Errorf("unable to update deposit "+
			"confirmations: %w", err)
	}

	// If the same outpoint reappeared after a transient wallet-view miss,
	// reactivate the existing record before we consider it new or vanished.
	err = m.reviveReappearedDeposits(ctx, utxos, bestHeight)
	if err != nil {
		return 0, fmt.Errorf("unable to revive reappeared deposits: %w",
			err)
	}

	// After handling reappearances, only still-missing outpoints contribute
	// towards replacement detection.
	err = m.invalidateVanishedDeposits(ctx, utxos)
	if err != nil {
		return 0, fmt.Errorf("unable to invalidate vanished "+
			"deposits: %w", err)
	}

	newDeposits := m.filterNewDeposits(utxos)
	if len(newDeposits) == 0 {
		log.Tracef("No new deposits...")
		return 0, nil
	}

	for _, utxo := range newDeposits {
		deposit, err := m.createNewDeposit(ctx, utxo, bestHeight)
		if err != nil {
			return 0, fmt.Errorf("unable to retain new deposit: %w",
				err)
		}

		log.Debugf("Received deposit: %v", deposit)
		err = m.startDepositFsm(ctx, deposit)
		if err != nil {
			return 0, fmt.Errorf("unable to start new deposit FSM: %w",
				err)
		}
	}

	return len(newDeposits), nil
}

// ReconcileDeposits triggers a best-effort reconciliation pass and returns the
// number of newly discovered deposits. Recovery calls this after restoring the
// address because deposit FSM state is not serialized in backups; it must be
// rebuilt from lnd's current wallet view.
func (m *Manager) ReconcileDeposits(ctx context.Context) (int, error) {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	return m.reconcileDeposits(ctx)
}

// listUnspentWithBestHeight returns the wallet's current static-address UTXOs
// together with a stable chain tip height for any confirmed outputs.
func (m *Manager) listUnspentWithBestHeight(ctx context.Context) (
	[]*lnwallet.Utxo, int32, error) {

	utxos, err := m.cfg.AddressManager.ListUnspent(ctx, 0, MaxConfs)
	if err != nil {
		return nil, 0, fmt.Errorf("unable to list new deposits: %w", err)
	}

	needsBestHeight := false
	for _, utxo := range utxos {
		if utxo.Confirmations > 0 {
			needsBestHeight = true
			break
		}
	}

	if !needsBestHeight {
		return utxos, 0, nil
	}

	if m.cfg.ChainKit == nil {
		return nil, 0, errors.New("chain kit client required for " +
			"confirmed deposits")
	}

	const maxAttempts = 3
	for range maxAttempts {
		_, beforeHeight, err := m.cfg.ChainKit.GetBestBlock(ctx)
		if err != nil {
			return nil, 0, fmt.Errorf("unable to get best block "+
				"before listing deposits: %w", err)
		}

		utxos, err = m.cfg.AddressManager.ListUnspent(ctx, 0, MaxConfs)
		if err != nil {
			return nil, 0, fmt.Errorf("unable to list new deposits: %w",
				err)
		}

		_, afterHeight, err := m.cfg.ChainKit.GetBestBlock(ctx)
		if err != nil {
			return nil, 0, fmt.Errorf("unable to get best block "+
				"after listing deposits: %w", err)
		}

		if beforeHeight == afterHeight {
			m.currentHeight.Store(uint32(afterHeight))
			return utxos, afterHeight, nil
		}
	}

	return nil, 0, errors.New("unable to get stable best block while " +
		"listing deposits")
}

// createNewDeposit transforms the wallet utxo into a deposit struct and stores
// it in our database and manager memory.
func (m *Manager) createNewDeposit(ctx context.Context,
	utxo *lnwallet.Utxo, bestHeight int32) (*Deposit, error) {

	confirmationHeight, err := confirmationHeightForUtxo(
		bestHeight, utxo,
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

	addressID, err := m.cfg.AddressManager.GetStaticAddressID(
		ctx, utxo.PkScript,
	)
	if err != nil {
		return nil, err
	}

	deposit := &Deposit{
		ID:                   id,
		state:                Deposited,
		OutPoint:             utxo.OutPoint,
		Value:                utxo.Value,
		ConfirmationHeight:   confirmationHeight,
		TimeOutSweepPkScript: timeoutSweepPkScript,
		AddressParams:        addressParams,
		AddressID:            addressID,
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
// UTXO from a stable best-known chain tip. Unconfirmed UTXOs return 0.
func confirmationHeightForUtxo(bestHeight int32,
	utxo *lnwallet.Utxo) (int64, error) {

	if utxo.Confirmations <= 0 {
		return 0, nil
	}

	if bestHeight <= 0 {
		return 0, fmt.Errorf("invalid best height %d", bestHeight)
	}

	firstConfirmationHeight := int64(bestHeight) - utxo.Confirmations + 1
	if firstConfirmationHeight <= 0 {
		return 0, fmt.Errorf("invalid confirmation height %d for %v "+
			"with best height %d and %d confirmations",
			firstConfirmationHeight, utxo.OutPoint, bestHeight,
			utxo.Confirmations)
	}

	return firstConfirmationHeight, nil
}

// updateDepositConfirmations backfills first confirmation heights for deposits
// that were previously detected unconfirmed.
func (m *Manager) updateDepositConfirmations(ctx context.Context,
	utxos []*lnwallet.Utxo, bestHeight int32) error {

	for _, utxo := range utxos {
		if utxo.Confirmations <= 0 {
			continue
		}

		m.mu.Lock()
		deposit, ok := m.deposits[utxo.OutPoint]
		m.mu.Unlock()
		if !ok {
			continue
		}

		deposit.Lock()
		if deposit.ConfirmationHeight > 0 {
			deposit.Unlock()
			continue
		}
		deposit.Unlock()

		confirmationHeight, err := confirmationHeightForUtxo(
			bestHeight, utxo,
		)
		if err != nil {
			return err
		}

		deposit.Lock()
		if deposit.ConfirmationHeight > 0 {
			deposit.Unlock()
			continue
		}

		previousConfirmationHeight := deposit.ConfirmationHeight
		deposit.ConfirmationHeight = confirmationHeight

		err = m.cfg.Store.UpdateDeposit(ctx, deposit)
		if err != nil {
			deposit.ConfirmationHeight = previousConfirmationHeight
			deposit.Unlock()
			return err
		}

		deposit.Unlock()
	}

	return nil
}

// reviveReappearedDeposits reactivates deposits that were previously marked as
// replaced if the exact same outpoint reappears in the wallet view.
//
// This is the inverse of invalidateVanishedDeposits: it lets us
// recover from a transient ListUnspent gap without inventing a second record
// for the same outpoint.
func (m *Manager) reviveReappearedDeposits(ctx context.Context,
	utxos []*lnwallet.Utxo, bestHeight int32) error {

	type reviveCandidate struct {
		deposit *Deposit
		utxo    *lnwallet.Utxo
	}

	var candidates []reviveCandidate

	m.mu.Lock()
	for _, utxo := range utxos {
		delete(m.missingDeposits, utxo.OutPoint)

		deposit, ok := m.deposits[utxo.OutPoint]
		if !ok {
			continue
		}

		if _, active := m.activeDeposits[utxo.OutPoint]; active {
			continue
		}

		deposit.Lock()
		isReplaced := deposit.IsInStateNoLock(Replaced)
		deposit.Unlock()
		if !isReplaced {
			continue
		}

		candidates = append(candidates, reviveCandidate{
			deposit: deposit,
			utxo:    utxo,
		})
	}
	m.mu.Unlock()

	for _, candidate := range candidates {
		confirmationHeight, err := confirmationHeightForUtxo(
			bestHeight, candidate.utxo,
		)
		if err != nil {
			return err
		}

		deposit := candidate.deposit
		deposit.Lock()
		if !deposit.IsInStateNoLock(Replaced) {
			deposit.Unlock()
			continue
		}

		previousState := deposit.state
		previousConfirmationHeight := deposit.ConfirmationHeight
		deposit.ConfirmationHeight = confirmationHeight
		deposit.SetStateNoLock(Deposited)
		err = m.cfg.Store.UpdateDeposit(ctx, deposit)
		if err != nil {
			deposit.ConfirmationHeight = previousConfirmationHeight
			deposit.SetStateNoLock(previousState)
			deposit.Unlock()
			return err
		}

		deposit.Unlock()

		log.Infof("Reactivated deposit %v after it reappeared in "+
			"wallet view", deposit.OutPoint)

		err = m.startDepositFsm(ctx, deposit)
		if err != nil {
			return err
		}
	}

	return nil
}

// invalidateVanishedDeposits marks Deposited outputs as replaced once lnd no
// longer reports the outpoint in multiple consecutive wallet observations.
//
// This closes the gap between wallet state and our DB state when a persisted
// deposit later disappears from the wallet view, for example because an
// unconfirmed funding transaction was replaced or because a previously
// confirmed transaction was evicted by a deep reorg. We only invalidate
// deposits that are still in the plain Deposited state.
//
// That keeps the scope narrow: in-flight states like LoopingIn already have
// their own recovery/error handling.
func (m *Manager) invalidateVanishedDeposits(ctx context.Context,
	utxos []*lnwallet.Utxo) error {

	currentUtxos := make(map[wire.OutPoint]struct{}, len(utxos))
	for _, utxo := range utxos {
		currentUtxos[utxo.OutPoint] = struct{}{}
	}

	m.mu.Lock()
	candidates := make([]*Deposit, 0, len(m.deposits))
	for outpoint, deposit := range m.deposits {
		if _, ok := currentUtxos[outpoint]; ok {
			delete(m.missingDeposits, outpoint)
			continue
		}

		deposit.Lock()
		isVanishedDeposit := deposit.IsInStateNoLock(Deposited)
		deposit.Unlock()
		if !isVanishedDeposit {
			delete(m.missingDeposits, outpoint)
			continue
		}

		m.missingDeposits[outpoint]++
		if m.missingDeposits[outpoint] < vanishedDepositThreshold {
			log.Debugf("Waiting for another wallet observation before "+
				"marking deposit %v replaced", outpoint)

			continue
		}

		delete(m.missingDeposits, outpoint)
		candidates = append(candidates, deposit)
	}
	m.mu.Unlock()

	for _, deposit := range candidates {
		deposit.Lock()
		if !deposit.IsInStateNoLock(Deposited) {
			deposit.Unlock()
			continue
		}

		// Persist the replacement marker before removing the deposit from the
		// active set so restarted clients and RPC consumers see the same outcome.
		previousState := deposit.state
		deposit.SetStateNoLock(Replaced)
		err := m.cfg.Store.UpdateDeposit(ctx, deposit)
		if err != nil {
			deposit.SetStateNoLock(previousState)
			deposit.Unlock()
			return err
		}

		deposit.Unlock()

		m.removeActiveDeposit(deposit.OutPoint)

		log.Infof("Marked vanished deposit %v as replaced",
			deposit.OutPoint)
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
// heavy things or call external endpoints that can block for a long time as
// this function blocks until the expectedFinalState is reached. The default
// timeout for the transition is set to DefaultTransitionTimeout.
func (m *Manager) TransitionDeposits(ctx context.Context, deposits []*Deposit,
	event fsm.EventType, expectedFinalState fsm.StateType) error {

	outpoints := make([]wire.OutPoint, len(deposits))
	for i, d := range deposits {
		outpoints[i] = d.OutPoint
	}

	m.mu.Lock()
	stateMachines, _ := m.toActiveDeposits(&outpoints)
	m.mu.Unlock()

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

// GetAllDeposits returns all active deposits.
func (m *Manager) GetAllDeposits(ctx context.Context) ([]*Deposit, error) {
	deposits, err := m.cfg.Store.AllDeposits(ctx)
	if err != nil {
		return nil, err
	}

	err = m.hydrateLegacyDepositAddressParams(ctx, deposits...)
	if err != nil {
		return nil, err
	}

	return deposits, nil
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

		err = m.hydrateLegacyDepositAddressParams(ctx, deposit)
		if err != nil {
			return nil, err
		}

		deposits = append(deposits, deposit)
	}

	return deposits, nil
}
