package deposit

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
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
	// Deposited signals that funds at a static address have reached the
	// confirmation height.
	Deposited = fsm.StateType("Deposited")

	// Withdrawing signals that the withdrawal transaction has been
	// broadcast, awaiting sufficient confirmations.
	Withdrawing = fsm.StateType("Withdrawing")

	// Withdrawn signals that the withdrawal transaction has been confirmed.
	Withdrawn = fsm.StateType("Withdrawn")

	// OpeningChannel signals that the open channel transaction has been
	// broadcast.
	OpeningChannel = fsm.StateType("OpeningChannel")

	// ChannelPublished signals that the open channel transaction has been
	// published and that the channel should be managed from lnd from now
	// on.
	ChannelPublished = fsm.StateType("ChannelPublished")

	// LoopingIn signals that the deposit is locked for a loop in swap.
	LoopingIn = fsm.StateType("LoopingIn")

	// LoopedIn signals that the loop in swap has been successfully
	// completed. It implies that we signed the sweepless sweep tx for the
	// server.
	LoopedIn = fsm.StateType("LoopedIn")

	// SweepHtlcTimeout signals that the htlc timeout path is in the
	// process of being swept.
	SweepHtlcTimeout = fsm.StateType("SweepHtlcTimeout")

	// HtlcTimeoutSwept signals that the htlc timeout path has been swept.
	HtlcTimeoutSwept = fsm.StateType("HtlcTimeoutSwept")

	// PublishExpirySweep signals that the deposit has expired, and we are
	// in the process of publishing the expiry sweep transaction.
	PublishExpirySweep = fsm.StateType("PublishExpirySweep")

	// WaitForExpirySweep signals that the expiry sweep transaction has been
	// published, and we are waiting for it to be confirmed.
	WaitForExpirySweep = fsm.StateType("WaitForExpirySweep")

	// Expired signals that the deposit has expired and the expiry sweep
	// transaction has been confirmed sufficiently.
	Expired = fsm.StateType("Expired")
)

// Events.
var (
	// OnStart is sent to the fsm once the deposit outpoint has been
	// sufficiently confirmed. It transitions the fsm into the Deposited
	// state from where we can trigger a withdrawal, a loopin or an expiry.
	OnStart = fsm.EventType("OnStart")

	// OnWithdrawInitiated is sent to the fsm when a withdrawal has been
	// initiated.
	OnWithdrawInitiated = fsm.EventType("OnWithdrawInitiated")

	// OnWithdrawn is sent to the fsm when a withdrawal has been confirmed.
	OnWithdrawn = fsm.EventType("OnWithdrawn")

	// OnOpeningChannel is sent to the fsm when a channel open has been
	// initiated.
	OnOpeningChannel = fsm.EventType("OnOpeningChannel")

	// OnChannelPublished is sent to the fsm when a channel open has been
	// published. Loop has done its work here and the channel should now be
	// managed from lnd.
	OnChannelPublished = fsm.EventType("OnChannelPublished")

	// OnLoopInInitiated is sent to the fsm when a loop in has been
	// initiated.
	OnLoopInInitiated = fsm.EventType("OnLoopInInitiated")

	// OnSweepingHtlcTimeout is sent to the fsm when the htlc timeout path
	// is being swept. This indicates that the server didn't pay the swap
	// invoice, but the htlc tx was published, from we which we need to
	// sweep the htlc timeout path.
	OnSweepingHtlcTimeout = fsm.EventType("OnSweepingHtlcTimeout")

	// OnHtlcTimeoutSwept is sent to the fsm when the htlc timeout path has
	// been swept.
	OnHtlcTimeoutSwept = fsm.EventType("OnHtlcTimeoutSwept")

	// OnLoopedIn is sent to the fsm when the user intents to use the
	// deposit for a loop in swap.
	OnLoopedIn = fsm.EventType("OnLoopedIn")

	// OnExpiry is sent to the fsm when the deposit has expired.
	OnExpiry = fsm.EventType("OnExpiry")

	// OnExpiryPublished is sent to the fsm when the expiry sweep tx has
	// been published.
	OnExpiryPublished = fsm.EventType("OnExpiryPublished")

	// OnExpirySwept is sent to the fsm when the expiry sweep tx has been
	// confirmed.
	OnExpirySwept = fsm.EventType("OnExpirySwept")

	// OnRecover is sent to the fsm when it should recover from client
	// restart.
	OnRecover = fsm.EventType("OnRecover")
)

// FSM is the state machine that handles the instant out.
type FSM struct {
	*fsm.StateMachine

	cfg *ManagerConfig

	deposit *Deposit

	params *address.Parameters

	address *script.StaticAddress

	blockNtfnChan chan uint32

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

			case <-ctx.Done():
				return
			}
		}
	}(depoFsm)

	return depoFsm, nil
}

// handleBlockNotification inspects the current block height and sends the
// OnExpiry event to publish the expiry sweep transaction if the deposit timed
// out, or it republishes the expiry sweep transaction if it was not yet swept.
func (f *FSM) handleBlockNotification(ctx context.Context,
	currentHeight uint32) {

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
	return fsm.States{
		fsm.EmptyState: fsm.State{
			Transitions: fsm.Transitions{
				OnStart: Deposited,
			},
			Action: fsm.NoOpAction,
		},
		Deposited: fsm.State{
			Transitions: fsm.Transitions{
				OnExpiry:            PublishExpirySweep,
				OnWithdrawInitiated: Withdrawing,
				OnLoopInInitiated:   LoopingIn,
				OnOpeningChannel:    OpeningChannel,
				// We encounter OnSweepingHtlcTimeout if the
				// server published the htlc tx without paying
				// us. We then need to monitor for the timeout
				// path to open up to sweep it.
				OnSweepingHtlcTimeout: SweepHtlcTimeout,
				OnRecover:             Deposited,
				fsm.OnError:           Deposited,
			},
			Action: fsm.NoOpAction,
		},
		PublishExpirySweep: fsm.State{
			Transitions: fsm.Transitions{
				OnRecover:         PublishExpirySweep,
				OnExpiryPublished: WaitForExpirySweep,
				// If the timeout sweep failed we go back to
				// Deposited, hoping that another timeout sweep
				// attempt will be successful. Alternatively,
				// the client can try to coop-spend the deposit.
				fsm.OnError: Deposited,
			},
			Action: f.PublishDepositExpirySweepAction,
		},
		WaitForExpirySweep: fsm.State{
			Transitions: fsm.Transitions{
				OnExpirySwept: Expired,
				// Upon recovery, we republish the sweep tx.
				OnRecover: PublishExpirySweep,
				// If the timeout sweep failed we go back to
				// Deposited, hoping that another timeout sweep
				// attempt will be successful. Alternatively,
				// the client can try to coop-spend the deposit.
				fsm.OnError: Deposited,
			},
			Action: f.WaitForExpirySweepAction,
		},
		Expired: fsm.State{
			Transitions: fsm.Transitions{
				OnExpiry: Expired,
			},
			Action: f.FinalizeDepositAction,
		},
		Withdrawing: fsm.State{
			Transitions: fsm.Transitions{
				OnWithdrawn: Withdrawn,
				// OnWithdrawInitiated is sent if a fee bump was
				// requested and the withdrawal was republished.
				OnWithdrawInitiated: Withdrawing,

				// Upon recovery, we remain in the Withdrawing
				// state so that the withdrawal manager can
				// reinstate the withdrawal.
				OnRecover: Withdrawing,

				// A precondition for the Withdrawing state is
				// that the withdrawal transaction has been
				// broadcast. If the deposit expires while the
				// withdrawal isn't confirmed, we can ignore the
				// expiry.
				OnExpiry: Withdrawing,

				// If the withdrawal failed we go back to
				// Deposited, hoping that another withdrawal
				// attempt will be successful. Alternatively,
				// the client can wait for the timeout sweep.
				fsm.OnError: Deposited,
			},
			Action: fsm.NoOpAction,
		},
		LoopingIn: fsm.State{
			Transitions: fsm.Transitions{
				// This event is triggered when the loop in
				// payment has been received. We consider the
				// swap to be completed and transition to a
				// final state.
				OnLoopedIn: LoopedIn,

				// If the deposit expires while the loop in is
				// still pending, we publish the expiry sweep.
				OnExpiry: PublishExpirySweep,

				OnLoopInInitiated: LoopingIn,

				OnRecover:   LoopingIn,
				fsm.OnError: Deposited,
			},
			Action: fsm.NoOpAction,
		},
		LoopedIn: fsm.State{
			Transitions: fsm.Transitions{
				OnExpiry: Expired,
			},
			Action: f.FinalizeDepositAction,
		},
		SweepHtlcTimeout: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcTimeoutSwept: HtlcTimeoutSwept,
				OnRecover:          SweepHtlcTimeout,
			},
			Action: fsm.NoOpAction,
		},
		HtlcTimeoutSwept: fsm.State{
			Transitions: fsm.Transitions{
				OnExpiry: HtlcTimeoutSwept,
			},
			Action: f.FinalizeDepositAction,
		},
		Withdrawn: fsm.State{
			Transitions: fsm.Transitions{
				OnExpiry:    Expired,
				OnWithdrawn: Withdrawn,
			},
			Action: f.FinalizeDepositAction,
		},
		OpeningChannel: fsm.State{
			Transitions: fsm.Transitions{
				fsm.OnError:        Deposited,
				OnChannelPublished: ChannelPublished,
				OnRecover:          OpeningChannel,
				// OnExpiry can arrive on every block once
				// the deposit is expired. Since the channel
				// open is still in progress we absorb the
				// event as a self-transition.
				OnExpiry: OpeningChannel,
			},
			Action: fsm.NoOpAction,
		},
		ChannelPublished: fsm.State{
			Transitions: fsm.Transitions{
				// OnExpiry can arrive on every block once
				// the deposit is expired. Since the channel
				// is already published we absorb the event
				// as a self-transition.
				OnExpiry: ChannelPublished,
			},
			Action: f.FinalizeDepositAction,
		},
	}
}

// DepositEntryFunction is called after every action and updates the deposit in
// the db.
func (f *FSM) updateDeposit(ctx context.Context,
	notification fsm.Notification) {

	if f.deposit == nil {
		return
	}

	type checkStateFunc func(state fsm.StateType) bool
	type setStateFunc func(state fsm.StateType)
	checkFunc := checkStateFunc(f.deposit.IsInState)
	setFunc := setStateFunc(f.deposit.SetState)
	if _, ok := lockedEvents[notification.Event]; ok {
		checkFunc = f.deposit.IsInStateNoLock
		setFunc = f.deposit.SetStateNoLock
	}

	setFunc(notification.NextState)
	if isUpdateSkipped(notification, checkFunc) {
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
func (f *FSM) Infof(format string, args ...interface{}) {
	log.Infof(
		"Deposit %v: "+format,
		append(
			[]interface{}{f.deposit.OutPoint},
			args...,
		)...,
	)
}

// Debugf logs a debug message with the deposit outpoint.
func (f *FSM) Debugf(format string, args ...interface{}) {
	log.Debugf(
		"Deposit %v: "+format,
		append(
			[]interface{}{f.deposit.OutPoint},
			args...,
		)...,
	)
}

// Errorf logs an error message with the deposit outpoint.
func (f *FSM) Errorf(format string, args ...interface{}) {
	log.Errorf(
		"Deposit %v: "+format,
		append(
			[]interface{}{f.deposit.OutPoint},
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
