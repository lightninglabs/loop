package deposit

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit/flow"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	DefaultObserverSize = 20
)

var (
	ErrProtocolVersionNotSupported = errors.New("protocol version not " +
		"supported")

	// Withdrawal and loop-in transitions lock their respective deposits
	// themselves. We need to make sure that we don't lock the deposit
	// twice. For the events below we expect the deposits already locked.
	lockedEvents = map[fsm.EventType]struct{}{
		OnLoopInInitiated:     {},
		OnSweepingHtlcTimeout: {},
		OnHtlcTimeoutSwept:    {},
		OnLoopedIn:            {},
		fsm.OnError:           {},
		OnWithdrawInitiated:   {},
		OnWithdrawn:           {},
		OnOpeningChannel:      {},
		OnChannelPublished:    {},
	}
)

// States.
var (
	// Deposited signals that funds at a static address have been detected
	// and are available to the client.
	Deposited = fsm.StateType(flow.Deposited)

	// Withdrawing signals that the withdrawal transaction has been
	// broadcast, awaiting sufficient confirmations.
	Withdrawing = fsm.StateType(flow.Withdrawing)

	// Withdrawn signals that the withdrawal transaction has been confirmed.
	Withdrawn = fsm.StateType(flow.Withdrawn)

	// OpeningChannel signals that the open channel transaction has been
	// broadcast.
	OpeningChannel = fsm.StateType(flow.OpeningChannel)

	// ChannelPublished signals that the open channel transaction has been
	// published and that the channel should be managed from lnd from now
	// on.
	ChannelPublished = fsm.StateType(flow.ChannelPublished)

	// LoopingIn signals that the deposit is locked for a loop in swap.
	LoopingIn = fsm.StateType(flow.LoopingIn)

	// LoopedIn signals that the loop in swap has been successfully
	// completed. It implies that we signed the sweepless sweep tx for the
	// server.
	LoopedIn = fsm.StateType(flow.LoopedIn)

	// SweepHtlcTimeout signals that the htlc timeout path is in the
	// process of being swept.
	SweepHtlcTimeout = fsm.StateType(flow.SweepHtlcTimeout)

	// HtlcTimeoutSwept signals that the htlc timeout path has been swept.
	HtlcTimeoutSwept = fsm.StateType(flow.HtlcTimeoutSwept)

	// PublishExpirySweep signals that the deposit has expired, and we are
	// in the process of publishing the expiry sweep transaction.
	PublishExpirySweep = fsm.StateType(flow.PublishExpirySweep)

	// WaitForExpirySweep signals that the expiry sweep transaction has been
	// published, and we are waiting for it to be confirmed.
	WaitForExpirySweep = fsm.StateType(flow.WaitForExpirySweep)

	// Expired signals that the deposit has expired and the expiry sweep
	// transaction has been confirmed sufficiently.
	Expired = fsm.StateType(flow.Expired)
)

// Events.
var (
	// OnStart is sent to the fsm once the deposit outpoint has been
	// detected. It transitions the fsm into the Deposited state from where
	// we can trigger a withdrawal, a loopin or an expiry.
	OnStart = fsm.EventType(flow.OnStart)

	// OnWithdrawInitiated is sent to the fsm when a withdrawal has been
	// initiated.
	OnWithdrawInitiated = fsm.EventType(flow.OnWithdrawInitiated)

	// OnWithdrawn is sent to the fsm when a withdrawal has been confirmed.
	OnWithdrawn = fsm.EventType(flow.OnWithdrawn)

	// OnOpeningChannel is sent to the fsm when a channel open has been
	// initiated.
	OnOpeningChannel = fsm.EventType(flow.OnOpeningChannel)

	// OnChannelPublished is sent to the fsm when a channel open has been
	// published. Loop has done its work here and the channel should now be
	// managed from lnd.
	OnChannelPublished = fsm.EventType(flow.OnChannelPublished)

	// OnLoopInInitiated is sent to the fsm when a loop in has been
	// initiated.
	OnLoopInInitiated = fsm.EventType(flow.OnLoopInInitiated)

	// OnSweepingHtlcTimeout is sent to the fsm when the htlc timeout path
	// is being swept. This indicates that the server didn't pay the swap
	// invoice, but the htlc tx was published, from we which we need to
	// sweep the htlc timeout path.
	OnSweepingHtlcTimeout = fsm.EventType(flow.OnSweepingHtlcTimeout)

	// OnHtlcTimeoutSwept is sent to the fsm when the htlc timeout path has
	// been swept.
	OnHtlcTimeoutSwept = fsm.EventType(flow.OnHtlcTimeoutSwept)

	// OnLoopedIn is sent to the fsm when the user intents to use the
	// deposit for a loop in swap.
	OnLoopedIn = fsm.EventType(flow.OnLoopedIn)

	// OnExpiry is sent to the fsm when the deposit has expired.
	OnExpiry = fsm.EventType(flow.OnExpiry)

	// OnExpiryPublished is sent to the fsm when the expiry sweep tx has
	// been published.
	OnExpiryPublished = fsm.EventType(flow.OnExpiryPublished)

	// OnExpirySwept is sent to the fsm when the expiry sweep tx has been
	// confirmed.
	OnExpirySwept = fsm.EventType(flow.OnExpirySwept)

	// OnRecover is sent to the fsm when it should recover from client
	// restart.
	OnRecover = fsm.EventType(flow.OnRecover)
)

// FSM is the state machine that handles the instant out.
type FSM struct {
	*fsm.StateMachine

	cfg *ManagerConfig

	deposit *Deposit

	params *script.Parameters

	address *script.StaticAddress

	blockNtfnChan chan uint32

	// stopChan requests shutdown of the block notification loop.
	stopChan chan struct{}

	// stopOnce ensures Stop is idempotent.
	stopOnce sync.Once

	// quitChan stops after the FSM stops consuming blockNtfnChan.
	quitChan chan struct{}

	// finalizedDepositChan is used to signal that the deposit has been
	// finalized and the FSM can be removed from the manager's memory.
	finalizedDepositChan chan wire.OutPoint
}

// NewFSM creates a new state machine that can action on all static address
// feature requests.
func NewFSM(ctx context.Context, deposit *Deposit, cfg *ManagerConfig,
	finalizedDepositChan chan wire.OutPoint,
	recoverStateMachine bool) (*FSM, error) {

	params, err := cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to get static address "+
			"parameters: %w", err)
	}

	address, err := cfg.AddressManager.GetStaticAddress(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to get static address: %w", err)
	}

	depoFsm := &FSM{
		cfg:                  cfg,
		deposit:              deposit,
		params:               params,
		address:              address,
		blockNtfnChan:        make(chan uint32),
		stopChan:             make(chan struct{}),
		quitChan:             make(chan struct{}),
		finalizedDepositChan: finalizedDepositChan,
	}

	depositStates := depoFsm.DepositStatesV0()
	switch params.ProtocolVersion {
	case version.ProtocolVersion_V0:

	default:
		return nil, ErrProtocolVersionNotSupported
	}

	if recoverStateMachine {
		depoFsm.StateMachine = fsm.NewStateMachineWithState(
			depositStates, deposit.GetState(),
			DefaultObserverSize,
		)
	} else {
		depoFsm.StateMachine = fsm.NewStateMachine(
			depositStates, DefaultObserverSize,
		)
	}

	depoFsm.ActionEntryFunc = depoFsm.updateDeposit

	go func(fsm *FSM) {
		defer close(fsm.quitChan)

		for {
			select {
			case currentHeight := <-fsm.blockNtfnChan:
				depoFsm.handleBlockNotification(
					ctx, currentHeight,
				)

			case <-fsm.stopChan:
				return

			case <-ctx.Done():
				return
			}
		}
	}(depoFsm)

	return depoFsm, nil
}

// Stop requests shutdown of the FSM's block notification loop.
func (f *FSM) Stop() {
	if f == nil || f.stopChan == nil {
		return
	}

	f.stopOnce.Do(func() {
		close(f.stopChan)
	})
}

// handleBlockNotification inspects the current block height and sends the
// OnExpiry event to publish the expiry sweep transaction if the deposit timed
// out, or it republishes the expiry sweep transaction if it was not yet swept.
func (f *FSM) handleBlockNotification(ctx context.Context,
	currentHeight uint32) {

	if f.deposit.IsInFinalState() {
		return
	}

	// If the deposit is expired but not yet sufficiently confirmed, we
	// republish the expiry sweep transaction.
	if f.deposit.IsExpired(currentHeight, f.params.Expiry) {
		if f.deposit.IsInState(WaitForExpirySweep) {
			f.PublishDepositExpirySweepAction(ctx, nil)
		} else {
			go func() {
				err := f.SendEvent(ctx, OnExpiry, nil)
				if err != nil {
					log.Debugf("error sending OnExpiry "+
						"event: %v", err)
				}
			}()
		}
	}
}

// DepositStatesV0 returns the states a deposit can be in.
func (f *FSM) DepositStatesV0() fsm.States {
	actions := map[flow.State]fsm.Action{
		flow.PublishExpirySweep: f.PublishDepositExpirySweepAction,
		flow.WaitForExpirySweep: f.WaitForExpirySweepAction,
		flow.Expired:            f.FinalizeDepositAction,
		flow.LoopedIn:           f.FinalizeDepositAction,
		flow.HtlcTimeoutSwept:   f.FinalizeDepositAction,
		flow.Withdrawn:          f.FinalizeDepositAction,
		flow.ChannelPublished:   f.FinalizeDepositAction,
	}
	states := make(fsm.States)
	for state, events := range flow.States(flow.Bitcoin) {
		transitions := make(fsm.Transitions, len(events))
		for event, next := range events {
			transitions[fsm.EventType(event)] = fsm.StateType(next)
		}
		action := actions[state]
		if action == nil {
			action = fsm.NoOpAction
		}
		states[fsm.StateType(state)] = fsm.State{
			Transitions: transitions, Action: action,
		}
	}
	return states
}

// DepositEntryFunction is called after every action and updates the deposit in
// the db.
func (f *FSM) updateDeposit(ctx context.Context,
	notification fsm.Notification) {

	if f.deposit == nil {
		return
	}

	_, alreadyLocked := lockedEvents[notification.Event]
	if !alreadyLocked {
		f.deposit.Lock()
		defer f.deposit.Unlock()
	}

	f.deposit.setStateNoLock(notification.NextState)
	if isUpdateSkipped(notification, f.deposit.isInStateNoLock) {
		return
	}

	f.Debugf("NextState: %v, PreviousState: %v, Event: %v",
		notification.NextState, notification.PreviousState,
		notification.Event,
	)

	err := f.cfg.Store.UpdateDeposit(ctx, f.deposit)
	if err != nil {
		f.Errorf("unable to update deposit: %v", err)
	}
}

// isUpdateSkipped returns true if the deposit should not be updated for the given
// notification.
func isUpdateSkipped(notification fsm.Notification,
	checkStateFunc func(stateType fsm.StateType) bool) bool {

	prevState := notification.PreviousState

	// Skip if we are in the empty state because no deposit has been
	// persisted yet.
	if checkStateFunc(fsm.EmptyState) {
		return true
	}

	// If we transitioned from the empty state to Deposited there's still no
	// deposit persisted, so we don't need to update it.
	if prevState == fsm.EmptyState && checkStateFunc(Deposited) {
		return true
	}

	// We don't update in self-loops, e.g. in the case of recovery.
	if checkStateFunc(prevState) {
		return true
	}

	return false
}

// Infof logs an info message with the deposit outpoint.
func (f *FSM) Infof(format string, args ...any) {
	log.Infof(
		"Deposit %v: "+format,
		append(
			[]any{f.deposit.OutPoint},
			args...,
		)...,
	)
}

// Debugf logs a debug message with the deposit outpoint.
func (f *FSM) Debugf(format string, args ...any) {
	log.Debugf(
		"Deposit %v: "+format,
		append(
			[]any{f.deposit.OutPoint},
			args...,
		)...,
	)
}

// Errorf logs an error message with the deposit outpoint.
func (f *FSM) Errorf(format string, args ...any) {
	log.Errorf(
		"Deposit %v: "+format,
		append(
			[]any{f.deposit.OutPoint},
			args...,
		)...,
	)
}

// SignDescriptor returns the sign descriptor for the static address output.
func (f *FSM) SignDescriptor(ctx context.Context) (*lndclient.SignDescriptor,
	error) {

	address, err := f.cfg.AddressManager.GetStaticAddress(ctx)
	if err != nil {
		return nil, err
	}

	return &lndclient.SignDescriptor{
		WitnessScript: address.TimeoutLeaf.Script,
		KeyDesc: keychain.KeyDescriptor{
			PubKey: f.params.ClientPubkey,
		},
		Output: wire.NewTxOut(
			int64(f.deposit.Value), f.params.PkScript,
		),
		HashType:   txscript.SigHashDefault,
		InputIndex: 0,
		SignMethod: input.TaprootScriptSpendSignMethod,
	}, nil
}
