package deposit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
)

// ErrWithdrawalReconciliationRequired fences deposits whose durable withdrawal
// update could not be read back. They cannot be selected or expired until the
// authoritative rows have been reconciled.
var ErrWithdrawalReconciliationRequired = errors.New(
	"deposit withdrawal reconciliation required",
)

// lockFSMEvents takes runtime event gates in canonical outpoint order, before
// any deposit locks. The manager holds mu while calling this helper.
func lockFSMEvents(ctx context.Context, machines []*FSM) (func(), error) {
	ordered := slices.Clone(machines)
	slices.SortFunc(ordered, func(a, b *FSM) int {
		return strings.Compare(a.deposit.OutPoint.String(), b.deposit.OutPoint.String())
	})
	var locked []*FSM
	unlock := func() {
		for _, machine := range slices.Backward(locked) {
			machine.releaseEvent()
		}
	}
	admissionCtx, cancel := context.WithTimeout(ctx, DefaultTransitionTimeout)
	defer cancel()
	for _, machine := range ordered {
		if err := machine.acquireEvent(admissionCtx); err != nil {
			unlock()
			return nil, err
		}
		locked = append(locked, machine)
	}
	return unlock, nil
}

// WithWithdrawalLock excludes competing deposit events while a withdrawal's
// domain transaction updates claims and state. The callback receives detached
// snapshots and must only perform the database transaction: no signing, network
// work, or callbacks into this manager. The persisted rows are read back before
// selection resumes, including after an error or panic with an uncertain write.
func (m *Manager) WithWithdrawalLock(ctx context.Context,
	outpoints []wire.OutPoint, update func([]*Deposit) error) error {

	if update == nil {
		return errors.New("withdrawal update callback is nil")
	}
	return m.withWithdrawalDeposits(ctx, outpoints, update)
}

// ReconcileWithdrawal synchronizes cached deposits and their legacy machines
// from authoritative withdrawal updates. It creates no state transitions or
// database history. Unknown outcomes remain fenced until a successful readback.
func (m *Manager) ReconcileWithdrawal(ctx context.Context,
	outpoints []wire.OutPoint) error {

	return m.withWithdrawalDeposits(ctx, outpoints, nil)
}

func (m *Manager) withWithdrawalDeposits(ctx context.Context,
	outpoints []wire.OutPoint, update func([]*Deposit) error) (err error) {

	if len(outpoints) == 0 {
		return errors.New("no withdrawal deposits")
	}
	if err := CheckDuplicates(outpoints); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	deposits := make([]*Deposit, 0, len(outpoints))
	machines := make([]*FSM, 0, len(outpoints))
	for _, outpoint := range outpoints {
		d, ok := m.deposits[outpoint]
		if !ok {
			return fmt.Errorf("unknown withdrawal deposit %v", outpoint)
		}
		machine := m.activeDeposits[outpoint]
		if update != nil && machine == nil {
			return fmt.Errorf("withdrawal deposit %v is inactive", outpoint)
		}
		deposits = append(deposits, d)
		if machine != nil {
			machines = append(machines, machine)
		}
	}
	unlockEvents, err := lockFSMEvents(ctx, machines)
	if err != nil {
		return err
	}
	defer unlockEvents()
	locked := lockDeposits(deposits)
	defer unlockDeposits(locked)
	if update != nil {
		for _, d := range deposits {
			machine := m.activeDeposits[d.OutPoint]
			if machine.withdrawalFenced {
				return ErrWithdrawalReconciliationRequired
			}
			switch d.getStateNoLock() {
			case Deposited, WithdrawalReserved, Withdrawing:
			default:
				return fmt.Errorf("deposit %v unavailable for withdrawal in %s",
					d.OutPoint, d.getStateNoLock())
			}
		}
	}
	for _, machine := range machines {
		machine.withdrawalFenced = true
	}
	defer func() {
		err = errors.Join(err, m.reconcileWithdrawalLocked(ctx, deposits))
	}()
	if update != nil {
		snapshots := make([]*Deposit, 0, len(deposits))
		for _, d := range deposits {
			snapshots = append(snapshots, cloneWithdrawalDeposit(d))
		}
		return update(snapshots)
	}
	return nil
}

// reconcileWithdrawalLocked holds Manager.mu, event gates and deposit locks.
func (m *Manager) reconcileWithdrawalLocked(ctx context.Context,
	deposits []*Deposit) error {

	loaded := make([]*Deposit, 0, len(deposits))
	for _, d := range deposits {
		stored, err := m.cfg.Store.GetDeposit(ctx, d.ID)
		if err != nil {
			return fmt.Errorf("%w: reload %v: %w",
				ErrWithdrawalReconciliationRequired, d.OutPoint, err)
		}
		if stored == nil || stored.ID != d.ID || stored.OutPoint != d.OutPoint ||
			stored.Value != d.Value ||
			!bytes.Equal(stored.TimeOutSweepPkScript, d.TimeOutSweepPkScript) {

			return fmt.Errorf("%w: mismatched deposit %v",
				ErrWithdrawalReconciliationRequired, d.OutPoint)
		}
		machine := m.activeDeposits[d.OutPoint]
		if machine != nil {
			if _, ok := machine.DepositStatesV0()[stored.getStateNoLock()]; !ok {
				return fmt.Errorf("%w: unknown state %s",
					ErrWithdrawalReconciliationRequired, stored.getStateNoLock())
			}
		}
		loaded = append(loaded, stored)
	}
	for i, d := range deposits {
		stored := loaded[i]
		copyWithdrawalDeposit(d, stored)
		machine := m.activeDeposits[d.OutPoint]
		if machine == nil {
			continue
		}
		machine.StateMachine = fsm.NewStateMachineWithState(
			machine.DepositStatesV0(), d.getStateNoLock(), DefaultObserverSize,
		)
		machine.ActionEntryFunc = machine.updateDeposit
		machine.withdrawalFenced = false
		if d.isInFinalStateNoLock() {
			delete(m.activeDeposits, d.OutPoint)
			machine.Stop()
		}
	}
	return nil
}

// The caller holds the source lock; these helpers deliberately exclude Mutex.
func cloneWithdrawalDeposit(source *Deposit) *Deposit {
	cloned := &Deposit{
		OutPoint:             source.OutPoint,
		ID:                   source.ID,
		Value:                source.Value,
		TimeOutSweepPkScript: bytes.Clone(source.TimeOutSweepPkScript),
	}
	copyWithdrawalDeposit(cloned, source)
	return cloned
}

func copyWithdrawalDeposit(destination, source *Deposit) {
	// Identity, value and timeout destination are immutable; readers may use
	// them without locking. Do not write them even when their values agree.
	destination.state = source.state
	destination.ConfirmationHeight = source.ConfirmationHeight
	destination.ExpirySweepTxid = source.ExpirySweepTxid
	destination.SwapHash = nil
	if source.SwapHash != nil {
		destination.SwapHash = new(*source.SwapHash)
	}
	destination.FinalizedWithdrawalTx = nil
	if source.FinalizedWithdrawalTx != nil {
		destination.FinalizedWithdrawalTx = source.FinalizedWithdrawalTx.Copy()
	}
	destination.OnchainCost = source.OnchainCost
}
