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

	PublishExpiredDeposit = fsm.StateType("PublishExpiredDeposit")

	WaitForExpirySweep = fsm.StateType("WaitForExpirySweep")

	Expired = fsm.StateType("Expired")

	Failed = fsm.StateType("Failed")
)

// Events.
var (
	OnStart           = fsm.EventType("OnStart")
	OnExpiry          = fsm.EventType("OnExpiry")
	OnExpiryPublished = fsm.EventType("OnExpiryPublished")
	OnExpirySwept     = fsm.EventType("OnExpirySwept")
	OnRecover         = fsm.EventType("OnRecover")
)

// FSM is the state machine that handles the instant out.
type FSM struct {
	*fsm.StateMachine

	cfg *ManagerConfig

	deposit *Deposit

	params *address.Parameters

	address *script.StaticAddress

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
			depositStates, deposit.getState(),
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
				err := depoFsm.handleBlockNotification(
					ctx, currentHeight,
				)
				if err != nil {
					log.Errorf("error handling block "+
						"notification: %v", err)
				}

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
func (f *FSM) handleBlockNotification(ctx context.Context,
	currentHeight uint32) error {

	params, err := f.cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return err
	}

	// If the deposit is expired but not yet sufficiently confirmed, we
	// republish the expiry sweep transaction.
	if f.deposit.isExpired(currentHeight, params.Expiry) {
		if f.deposit.isInState(WaitForExpirySweep) {
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

	return nil
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
				OnExpiry:  PublishExpiredDeposit,
				OnRecover: Deposited,
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
func (f *FSM) updateDeposit(ctx context.Context,
	notification fsm.Notification) {

	if f.deposit == nil {
		return
	}

	f.Debugf("NextState: %v, PreviousState: %v, Event: %v",
		notification.NextState, notification.PreviousState,
		notification.Event,
	)

	f.deposit.setState(notification.NextState)

	// Don't update the deposit if we are in an initial state or if we
	// are transitioning from an initial state to a failed state.
	d := f.deposit
	if d.isInState(fsm.EmptyState) || d.isInState(Deposited) ||
		(notification.PreviousState == Deposited && d.isInState(
			Failed,
		)) {

		return
	}

	err := f.cfg.Store.UpdateDeposit(ctx, f.deposit)
	if err != nil {
		f.Errorf("unable to update deposit: %v", err)
	}
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
