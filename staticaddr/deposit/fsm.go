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
)

// States.
var (
	Deposited = fsm.StateType("Deposited")

	Withdrawing = fsm.StateType("Withdrawing")

	Withdrawn = fsm.StateType("Withdrawn")

	LoopingIn = fsm.StateType("LoopingIn")

	LoopedIn = fsm.StateType("LoopedIn")

	SweepHtlcTimout = fsm.StateType("SweepHtlcTimout")

	HtlcTimeoutSwept = fsm.StateType("HtlcTimeoutSwept")

	PublishExpiredDeposit = fsm.StateType("PublishExpiredDeposit")

	WaitForExpirySweep = fsm.StateType("WaitForExpirySweep")

	Expired = fsm.StateType("Expired")

	Failed = fsm.StateType("Failed")
)

// Events.
var (
	OnStart              = fsm.EventType("OnStart")
	OnWithdrawInitiated  = fsm.EventType("OnWithdrawInitiated")
	OnWithdrawn          = fsm.EventType("OnWithdrawn")
	OnLoopinInitiated    = fsm.EventType("OnLoopinInitiated")
	OnSweepingHtlcTimout = fsm.EventType("OnSweepingHtlcTimout")
	OnHtlcTimeoutSwept   = fsm.EventType("OnHtlcTimeoutSwept")
	OnLoopedIn           = fsm.EventType("OnLoopedIn")
	OnExpiry             = fsm.EventType("OnExpiry")
	OnExpiryPublished    = fsm.EventType("OnExpiryPublished")
	OnExpirySwept        = fsm.EventType("OnExpirySwept")
	OnRecover            = fsm.EventType("OnRecover")
)

// FSM is the state machine that handles the instant out.
type FSM struct {
	*fsm.StateMachine

	cfg *ManagerConfig

	deposit *Deposit

	params *address.Parameters

	address *script.StaticAddress

	ctx context.Context

	blockNtfnChan chan uint32

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
			"parameters: %v", err)
	}

	address, err := cfg.AddressManager.GetStaticAddress(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to get static address: %v", err)
	}

	depoFsm := &FSM{
		cfg:                  cfg,
		deposit:              deposit,
		params:               params,
		address:              address,
		ctx:                  ctx,
		blockNtfnChan:        make(chan uint32),
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

	go func() {
		for {
			select {
			case currentHeight := <-depoFsm.blockNtfnChan:
				depoFsm.handleBlockNotification(currentHeight)

			case <-ctx.Done():
				return
			}
		}
	}()

	return depoFsm, nil
}

// handleBlockNotification inspects the current block height and sends the
// OnExpiry event to publish the expiry sweep transaction if the deposit timed
// out, or it republishes the expiry sweep transaction if it was not yet swept.
func (f *FSM) handleBlockNotification(currentHeight uint32) {
	// If the deposit is expired but not yet sufficiently confirmed, we
	// republish the expiry sweep transaction.
	if f.deposit.IsExpired(currentHeight, f.params.Expiry) {
		if f.deposit.IsInState(WaitForExpirySweep) {
			f.PublishDepositExpirySweepAction(nil)
		} else {
			go func() {
				err := f.SendEvent(OnExpiry, nil)
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
				OnExpiry:            PublishExpiredDeposit,
				OnWithdrawInitiated: Withdrawing,
				OnLoopinInitiated:   LoopingIn,
				OnRecover:           Deposited,
			},
			Action: fsm.NoOpAction,
		},
		PublishExpiredDeposit: fsm.State{
			Transitions: fsm.Transitions{
				OnRecover:         PublishExpiredDeposit,
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
				OnRecover: PublishExpiredDeposit,
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
			Action: f.SweptExpiredDepositAction,
		},
		Withdrawing: fsm.State{
			Transitions: fsm.Transitions{
				OnWithdrawn: Withdrawn,
				// Upon recovery, we go back to the Deposited
				// state. The deposit by then has a withdrawal
				// address stamped to it which will cause it to
				// transition into the Withdrawing state again.
				OnRecover: Deposited,

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
				OnExpiry: PublishExpiredDeposit,

				// We encounter this signal if the server
				// published the htlc tx without paying us. We
				// then need to monitor for the timeout path to
				// open up to sweep it.
				OnSweepingHtlcTimout: SweepHtlcTimout,

				OnLoopinInitiated: LoopingIn,

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
		SweepHtlcTimout: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcTimeoutSwept: HtlcTimeoutSwept,
				OnRecover:          SweepHtlcTimout,
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
				OnExpiry: Expired,
			},
			Action: f.FinalizeDepositAction,
		},
		Failed: fsm.State{
			Transitions: fsm.Transitions{
				OnExpiry: Failed,
			},
			Action: fsm.NoOpAction,
		},
	}
}

// DepositEntryFunction is called after every action and updates the deposit in
// the db.
func (f *FSM) updateDeposit(notification fsm.Notification) {
	if f.deposit == nil {
		return
	}

	f.Debugf("NextState: %v, PreviousState: %v, Event: %v",
		notification.NextState, notification.PreviousState,
		notification.Event,
	)

	f.deposit.SetState(notification.NextState)

	if skipUpdate(notification, f.deposit) {
		return
	}

	err := f.cfg.Store.UpdateDeposit(f.ctx, f.deposit)
	if err != nil {
		f.Errorf("unable to update deposit: %v", err)
	}
}

// Don't update the deposit if we are in an initial state or if we are
// transitioning from an initial state to a failed state.
func skipUpdate(notification fsm.Notification, d *Deposit) bool {
	prevState := notification.PreviousState
	if d.IsInState(fsm.EmptyState) || d.IsInState(Deposited) &&
		prevState == fsm.EmptyState ||
		d.IsInState(prevState) ||
		prevState == Deposited && d.IsInState(Deposited) ||
		prevState == Deposited && d.IsInState(Failed) ||
		prevState == LoopingIn && d.IsInState(LoopingIn) {

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
func (f *FSM) SignDescriptor() (*lndclient.SignDescriptor, error) {
	address, err := f.cfg.AddressManager.GetStaticAddress(f.ctx)
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
