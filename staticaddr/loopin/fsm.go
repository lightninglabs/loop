package loopin

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Config contains the services required for the instant out FSM.
type Config struct {
	// StaticAddressServerClient is the client that calls the swap server
	// rpcs to negotiate static address withdrawals.
	StaticAddressServerClient swapserverrpc.StaticAddressServerClient

	// AddressManager gives the withdrawal manager access to static address
	// parameters.
	AddressManager AddressManager

	// DepositManager gives the withdrawal manager access to the deposits
	// enabling it to create and manage withdrawals.
	DepositManager DepositManager

	WithdrawalManager WithdrawalManager

	LndClient lndclient.LightningClient

	InvoicesClient lndclient.InvoicesClient

	SwapClient *loop.Client

	NodePubkey route.Vertex

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

	// Store is the database store that is used to store static address
	// related records.
	Store SqlStore
}

// FSM embeds an FSM and extends it with a static loop-in and a config.
// It is used to run a reservation state machine.
type FSM struct {
	*fsm.StateMachine

	cfg *Config

	loopIn *StaticAddressLoopIn

	ctx context.Context

	// htlcMusig2Sessions contains all the reservations input musig2
	// sessions that will be used for the htlc transaction.
	htlcMusig2Sessions []*input.MuSig2SessionInfo

	// htlcClientNonces contains all the nonces that the client sent for
	// the htlc musig2 sessions.
	htlcClientNonces [][musig2.PubNonceSize]byte

	// htlcServerNonces contains all the nonces that the server generated
	// for the htlc musig2 sessions.
	htlcServerNonces [][musig2.PubNonceSize]byte

	// htlcClientSigs contains all the signatures that the client generated
	// for the htlc musig2 sessions.
	htlcClientSigs [][]byte

	// sweeplessMusig2Sessions ...
	sweeplessMusig2Sessions []*input.MuSig2SessionInfo

	// sweeplessClientNonces ...
	sweeplessClientNonces [][musig2.PubNonceSize]byte

	// sweeplessServerNonces ...
	sweeplessServerNonces [][musig2.PubNonceSize]byte

	// sweeplessClientSigs ...
	sweeplessClientSigs [][]byte
}

// NewFSM creates...
func NewFSM(ctx context.Context, cfg *Config) *FSM {
	loopInFsm := &FSM{
		ctx:    ctx,
		cfg:    cfg,
		loopIn: &StaticAddressLoopIn{},
	}

	loopInFsm.StateMachine = fsm.NewStateMachine(loopInFsm.GetStates(), 20)
	loopInFsm.ActionEntryFunc = loopInFsm.updateLoopIn

	return loopInFsm
}

// States. The full FSM diagram can be seen in the reservation/fsm.md file.
var (
	ValidateRequest    = fsm.StateType("ValidateRequest")
	SignHtlcTx         = fsm.StateType("SignHtlcTx")
	MonitorInvoice     = fsm.StateType("MonitorInvoice")
	SignSweeplessSweep = fsm.StateType("SignSweeplessSweep")
	Succeeded          = fsm.StateType("Succeeded")
	Failed             = fsm.StateType("Failed")
)

// Events
var (
	OnNewRequest           = fsm.EventType("OnNewRequest")
	OnValidRequest         = fsm.EventType("OnValidRequest")
	OnHtlcTxSigned         = fsm.EventType("OnHtlcTxSigned")
	OnPaymentReceived      = fsm.EventType("OnPaymentReceived")
	OnSweeplessSweepSigned = fsm.EventType("OnSweeplessSweepSigned")
	OnRecover              = fsm.EventType("OnRecover")
)

// GetStates returns the state and transition map for the reservation state
// machine. The state map can be viewed in the fsm.md file.
func (f *FSM) GetStates() fsm.States {
	return fsm.States{
		fsm.EmptyState: fsm.State{
			Transitions: fsm.Transitions{
				OnNewRequest: ValidateRequest,
			},
			Action: fsm.NoOpAction,
		},
		ValidateRequest: fsm.State{
			Transitions: fsm.Transitions{
				OnValidRequest: SignHtlcTx,
				OnRecover:      Failed,
				fsm.OnError:    Failed,
			},
			Action: f.ValidateRequestAction,
		},
		SignHtlcTx: fsm.State{
			Transitions: fsm.Transitions{
				OnHtlcTxSigned: MonitorInvoice,
				OnRecover:      Failed,
				fsm.OnError:    Failed,
			},
			Action: f.SignHtlcTxAction,
		},
		MonitorInvoice: fsm.State{
			Transitions: fsm.Transitions{
				OnPaymentReceived: SignSweeplessSweep,
				OnRecover:         Failed,
				fsm.OnError:       Failed,
			},
			// TODO: WARNING: If the server publishes the HTLC
			// without paying the invoice we need to sweep the
			// timeout path.
			Action: f.MonitorInvoiceAction,
		},
		SignSweeplessSweep: fsm.State{
			Transitions: fsm.Transitions{
				OnSweeplessSweepSigned: Succeeded,
				OnRecover:              Failed,
				fsm.OnError:            Failed,
			},
			Action: f.SignSweeplessSweepAction,
		},
		Succeeded: fsm.State{
			Action: fsm.NoOpAction,
		},
		Failed: fsm.State{
			Action: fsm.NoOpAction,
		},
	}
}

// updateReservation is called after every action and updates the reservation
// in the db.
func (f *FSM) updateLoopIn(notification fsm.Notification) {
	f.Infof("Current: %v", notification.NextState)

	// Skip the update if the reservation is not yet initialized.
	if f.loopIn == nil {
		return
	}

	f.loopIn.SetState(notification.NextState)

	// If we're in the early stages we don't have created the loop-in in the
	// store yet and won't need to update it.
	if f.loopIn.GetState() == fsm.EmptyState {
		return
	}

	/*	err := l.cfg.Store.UpdateStaticAddressLoopIn(l.ctx, l.loopIn)
		if err != nil {
			log.Errorf("Error updating reservation: %v", err)
			return
		}

	*/
}

// Infof logs an info message with the reservation id.
func (f *FSM) Infof(format string, args ...interface{}) {
	if f.loopIn == nil {
		log.Infof(format, args...)
		return
	}
	log.Infof(
		"StaticAddr loop-in %x: %s", f.loopIn.ID,
		fmt.Sprintf(format, args...),
	)
}

// Debugf logs a debug message with the reservation id.
func (f *FSM) Debugf(format string, args ...interface{}) {
	if f.loopIn == nil {
		log.Infof(format, args...)
		return
	}
	log.Debugf(
		"StaticAddr loop-in %x: %s", f.loopIn.ID,
		fmt.Sprintf(format, args...),
	)
}

// Errorf logs an error message with the reservation id.
func (f *FSM) Errorf(format string, args ...interface{}) {
	if f.loopIn == nil {
		log.Errorf(format, args...)
		return
	}
	log.Errorf(
		"StaticAddr loop-in %x: %s", f.loopIn.ID,
		fmt.Sprintf(format, args...),
	)
}
