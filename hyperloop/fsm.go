package hyperloop

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// States represents the possible states of the hyperloop FSM.
var (
	// Init is the initial state of the hyperloop FSM.
	Init = fsm.StateType("Init")

	// Registering is the state where we try to pay the hyperloop invoice.
	Registering = fsm.StateType("Registering")

	// WaitForPublish is the state where we wait for the hyperloop output to be
	// published.
	WaitForPublish = fsm.StateType("WaitForPublish")

	// WaitForConfirmation is the state where we wait for the hyperloop output
	// to be confirmed.
	WaitForConfirmation = fsm.StateType("WaitForConfirmation")

	// PushHtlcNonce is the state where we push the htlc nonce to the server.
	PushHtlcNonce = fsm.StateType("PushHtlcNonce")

	// WaitForReadyForHtlcSig is the event that is triggered when the server
	// is ready to receive the htlc sig.
	WaitForReadyForHtlcSig = fsm.StateType("WaitForReadyForHtlcSig")

	// PushHtlcSig is the state where we push the htlc sig to the server.
	PushHtlcSig = fsm.StateType("PushHtlcSig")

	// WaitForHtlcSig is the state where we wait for the final htlc sig.
	WaitForHtlcSig = fsm.StateType("WaitForHtlcSig")

	// PushPreimage is the state where we push the preimage and sweepless sweep
	// nonce to the server.
	PushPreimage = fsm.StateType("PushPreimage")

	// WaitForReadyForSweeplessSweepSig is the event that is triggered when the
	// server is ready to receive the sweepless sweep sig.
	WaitForReadyForSweeplessSweepSig = fsm.StateType("WaitForReadyForSweeplessSweepSig")

	// PushSweeplessSweepSig is the state where we push the sweepless sweep sig
	// to the server.
	PushSweeplessSweepSig = fsm.StateType("PushSweeplessSweepSig")

	// WaitForSweepPublish is the state where we wait for the sweep transaction
	// to be published.
	WaitForSweepPublish = fsm.StateType("PubslishSweep")

	// WaitForSweepConfirmation is the state where we wait for the sweep
	// transaction to be confirmed.
	WaitForSweepConfirmation = fsm.StateType("WaitForSweepConfirmation")

	// SweepConfirmed is a final state where the sweep transaction has been
	// confirmed.
	SweepConfirmed = fsm.StateType("SweepConfirmed")

	// PublishHtlc is the state where we publish the htlc transaction.
	PublishHtlc = fsm.StateType("PublishHtlc")

	// WaitForHtlcConfirmation is the state where we wait for the htlc
	// transaction to be confirmed.
	WaitForHtlcConfirmation = fsm.StateType("WaitForHtlcConfirmation")

	// HtlcConfirmed is the state where the htlc transaction has been confirmed.
	HtlcConfirmed = fsm.StateType("HtlcConfirmed")

	// PublishHtlcSweep is the state where we publish the htlc sweep
	// transaction.
	PublishHtlcSweep = fsm.StateType("PublishHtlcSweep")

	// WaitForHtlcSweepConfirmation is the state where we wait for the htlc
	// sweep transaction to be confirmed.
	WaitForHtlcSweepConfirmation = fsm.StateType("WaitForHtlcSweepConfirmation")

	// HtlcSweepConfirmed is the state where the htlc sweep transaction has been
	// confirmed.
	HtlcSweepConfirmed = fsm.StateType("HtlcSweepConfirmed")

	// Failed is a final state where the hyperloop has failed.
	Failed = fsm.StateType("Failed")
)

// Events
var (
	// OnStart is the event that is sent when the hyperloop FSM is started.
	OnStart = fsm.EventType("OnStart")

	// OnInit is the event is sent when the FSM is initialized.
	OnInit = fsm.EventType("OnInit")

	// OnRegistered is the event that is triggered when we have been registered
	// with the server.
	OnRegistered = fsm.EventType("OnRegistered")

	// OnPublished is the event that is triggered when the hyperloop output has
	// been published.
	OnPublished = fsm.EventType("OnPublished")

	// OnConfirmed is the event that is triggered when the hyperloop output has
	// been confirmed.
	OnConfirmed = fsm.EventType("OnConfirmed")

	// OnPushedHtlcNonce is the event that is triggered when the htlc nonce has
	// been pushed to the server.
	OnPushedHtlcNonce = fsm.EventType("OnPushedHtlcNonce")

	// OnReadyForHtlcSig is the event that is triggered when the server is ready
	// to receive the htlc sig.
	OnReadyForHtlcSig = fsm.EventType("OnReadyForHtlcSig")

	// OnPushedHtlcSig is the event that is sent when the htlc sig has been
	// pushed to the server.
	OnPushedHtlcSig = fsm.EventType("OnPushedHtlcSig")

	// OnReceivedHtlcSig is the event that is sent when the htlc sig has been
	// received.
	OnReceivedHtlcSig = fsm.EventType("OnReceivedHtlcSig")

	// OnPushedPreimage is the event that is sent when the preimage has been
	// pushed to the server.
	OnPushedPreimage = fsm.EventType("OnPushedPreimage")

	// OnReadyForSweeplessSweepSig is the event that is sent when the server is
	// ready to receive the sweepless sweep sig.
	OnReadyForSweeplessSweepSig = fsm.EventType("OnReadyForSweeplessSweepSig")

	// OnPushedSweeplessSweepSig is the event that is sent when the sweepless
	// sweep sig has been pushed to the server.
	OnPushedSweeplessSweepSig = fsm.EventType("OnPushedSweeplessSweepSig")

	// OnSweeplessSweepPublish is the event that is sent when the sweepless
	// sweep transaction has been published.
	OnSweeplessSweepPublish = fsm.EventType("OnSweeplessSweepPublish")

	// OnSweeplessSweepConfirmed is the event that is sent when the sweepless
	// sweep transaction has been confirmed.
	OnSweeplessSweepConfirmed = fsm.EventType("OnSweeplessSweepConfirmed")

	// OnPublishHtlc is the event that is sent when we should publish the htlc
	// transaction.
	OnPublishHtlc = fsm.EventType("OnPublishHtlc")

	// OnHtlcPublished is the event that is sent when the htlc transaction has
	// been published.
	OnHtlcPublished = fsm.EventType("OnHtlcPublished")

	// OnHtlcConfirmed is the event that is sent when the htlc transaction has
	// been confirmed.
	OnHtlcConfirmed = fsm.EventType("OnHtlcConfirmed")

	// OnSweepHtlc is the event that is sent when we publish the htlc sweep
	// transaction.
	OnSweepHtlc = fsm.EventType("OnSweepHtlc")

	// OnSweepPublished is the event that is sent when the sweep transaction has
	// been published.
	OnSweepPublished = fsm.EventType("OnSweepPublished")

	// OnHtlcSweepConfirmed is the event that is sent when the htlc sweep
	// transaction has been confirmed.
	OnHtlcSweepConfirmed = fsm.EventType("OnHtlcSweepConfirmed")

	// OnHyperlppSpent is the event that is sent when the hyperloop output has
	// been spent.
	OnHyperloopSpent = fsm.EventType("OnHyperloopSpent")

	// OnPaymentFailed is the event that is sent when the payment has failed.
	OnPaymentFailed = fsm.EventType("OnPaymentFailed")
)

// FSMConfig contains the services required for the hyperloop FSM.
type FSMConfig struct {
	// Store is used to store the hyperloop.
	Store Store

	// LndClient is used to interact with the lnd node.
	LndClient lndclient.LightningClient

	// Signer is used to sign transactions.
	Signer lndclient.SignerClient

	// Wallet is used to derive keys.
	Wallet lndclient.WalletKitClient

	// ChainNotifier is used to listen for chain events.
	ChainNotifier lndclient.ChainNotifierClient

	// RouterClient is used to dispatch and track payments.
	RouterClient lndclient.RouterClient

	// Network is the network that is used for the swap.
	Network *chaincfg.Params
}

// FSM is the state machine that manages the hyperloop process.
type FSM struct {
	*fsm.StateMachine

	ctx context.Context

	cfg *Config

	// hyperloop contains all the data required for the hyperloop process.
	hyperloop *HyperLoop

	// spendSubOnce is used to ensure that we only subscribe to the spend
	// notification once.
	spendSubOnce *sync.Once

	// isConnectedToNotificationStream is true if we are connected to the
	// notification stream.
	isConnectedToNotificationStream bool

	// isConnectedMutex is used to ensure that we safely access the
	// isConnectedToNotificationStream variable.
	isConnectedMutex sync.Mutex

	// lastNotification is the last notification that we received.
	lastNotification *swapserverrpc.HyperloopNotificationStreamResponse
}

// NewFSM creates a new instant out FSM.
func NewFSM(ctx context.Context, cfg *Config) (*FSM, error) {

	hyperloop := &HyperLoop{
		State: fsm.EmptyState,
	}

	return NewFSMFromHyperloop(ctx, cfg, hyperloop)
}

// NewFSMFromHyperloop creates a new instantout FSM from an existing instantout
// recovered from the database.
func NewFSMFromHyperloop(ctx context.Context, cfg *Config,
	hyperloop *HyperLoop) (*FSM, error) {

	instantOutFSM := &FSM{
		ctx:          ctx,
		cfg:          cfg,
		hyperloop:    hyperloop,
		spendSubOnce: new(sync.Once),
	}

	instantOutFSM.ActionEntryFunc = instantOutFSM.updateHyperloop

	return instantOutFSM, nil
}

// GetHyperloopStates returns the statemap that defines the hyperloop FSM.
func (f *FSM) GetStateMap() fsm.States {
	return fsm.States{
		fsm.EmptyState: fsm.State{
			Transitions: fsm.Transitions{
				OnStart: Init,
			},
			Action: nil,
		},
		Init: fsm.State{
			Transitions: fsm.Transitions{
				OnInit:      Registering,
				fsm.OnError: Failed,
			},
			Action: f.initHyperloopAction,
		},
		Registering: fsm.State{
			Transitions: fsm.Transitions{
				OnRegistered: WaitForPublish,
				fsm.OnError:  Failed,
			},
			Action: f.registerHyperloopAction,
		},
		WaitForPublish: fsm.State{
			Transitions: fsm.Transitions{
				OnPublished: WaitForConfirmation,
				fsm.OnError: Failed,
			},
			Action: f.waitForPublishAction,
		},
		WaitForConfirmation: fsm.State{
			Transitions: fsm.Transitions{
				OnConfirmed: PushHtlcNonce,
				fsm.OnError: Failed,
			},
			Action: f.waitForConfirmationAction,
		},
		PushHtlcNonce: fsm.State{
			Transitions: fsm.Transitions{
				OnPushedHtlcNonce: WaitForReadyForHtlcSig,
				fsm.OnError:       Failed,
			},
			Action: f.pushHtlcNonceAction,
		},
		WaitForReadyForHtlcSig: fsm.State{
			Transitions: fsm.Transitions{
				OnReadyForHtlcSig: PushHtlcSig,
				fsm.OnError:       Failed,
			},
			Action: f.waitForReadyForHtlcSignAction,
		},
		PushHtlcSig: fsm.State{
			Transitions: fsm.Transitions{
				OnPushedHtlcSig: WaitForHtlcSig,
				fsm.OnError:     Failed,
			},
			Action: f.pushHtlcSigAction,
		},
		WaitForHtlcSig: fsm.State{
			Transitions: fsm.Transitions{
				OnReceivedHtlcSig: PushPreimage,
				fsm.OnError:       Failed,
			},
			Action: f.waitForHtlcSig,
		},
		PushPreimage: fsm.State{
			Transitions: fsm.Transitions{
				OnPushedPreimage: WaitForReadyForSweeplessSweepSig,
				fsm.OnError:      Failed,
			},
			Action: f.pushPreimageAction,
		},
		WaitForReadyForSweeplessSweepSig: fsm.State{
			Transitions: fsm.Transitions{
				OnReadyForSweeplessSweepSig: PushSweeplessSweepSig,
				fsm.OnError:                 Failed,
			},
			Action: f.waitForReadyForSweepAction,
		},
		PushSweeplessSweepSig: fsm.State{
			Transitions: fsm.Transitions{
				OnPushedSweeplessSweepSig: WaitForSweepPublish,
				fsm.OnError:               Failed,
			},
			Action: f.pushSweepSigAction,
		},
		WaitForSweepPublish: fsm.State{
			Transitions: fsm.Transitions{
				OnSweeplessSweepPublish: WaitForSweepConfirmation,
				fsm.OnError:             Failed,
			},
			Action: fsm.NoOpAction,
		},
		WaitForSweepConfirmation: fsm.State{
			Transitions: fsm.Transitions{
				OnSweeplessSweepConfirmed: SweepConfirmed,
				fsm.OnError:               Failed,
			},
			Action: f.waitForSweeplessSweepConfirmationAction,
		},
		SweepConfirmed: fsm.State{
			Action: fsm.NoOpAction,
		},
		// todo htlc states
		Failed: fsm.State{
			Action: fsm.NoOpAction,
		},
	}
}

// updateHyperloop updates the hyperloop in the database. This function
// is called after every new state transition.
func (r *FSM) updateHyperloop(notification fsm.Notification) {
	if r.hyperloop == nil {
		return
	}

	r.Debugf(
		"NextState: %v, PreviousState: %v, Event: %v",
		notification.NextState, notification.PreviousState,
		notification.Event,
	)

	r.hyperloop.State = notification.NextState

	// Don't update the reservation if we are in an initial state or if we
	// are transitioning from an initial state to a failed state.
	if r.hyperloop.State == fsm.EmptyState ||
		r.hyperloop.State == Init ||
		(notification.PreviousState == Init &&
			r.hyperloop.State == Failed) {

		return
	}

	// Subscribe to the hyperloop notifications.
	r.subscribeHyperloopNotifications()

	// Subscribe to the spend notification of the current outpoint.
	err := r.subscribeHyperloopOutpointSpend()
	if err != nil {
		r.Errorf("unable to subscribe to spend notification: %v", err)
	}

	err = r.cfg.Store.UpdateHyperloop(r.ctx, r.hyperloop)
	if err != nil {
		r.Errorf("unable to update hyperloop: %v", err)
	}
}

func (f *FSM) subscribeHyperloopOutpointSpend() error {
	if isFinalState(f.hyperloop.State) {
		return nil
	}
	// If we don't have an outpoint yet, we can't subscribe to it.
	if f.hyperloop.ConfirmedOutpoint == nil {
		return nil
	}

	// We only want to subscribe once, so we use a sync.Once to ensure that.
	f.spendSubOnce.Do(func() {
		spendChan, errChan, err := f.cfg.ChainNotifier.RegisterSpendNtfn(
			f.ctx, f.hyperloop.ConfirmedOutpoint, nil,
			f.hyperloop.ConfirmationHeight,
		)
		if err != nil {
			f.Errorf("unable to subscribe to spend notification: %v", err)
			return
		}

		go func() {
			select {
			case spend := <-spendChan:
				spendEvent := f.getHyperloopSpendingEvent(spend)
				err := f.SendEvent(spendEvent, nil)
				if err != nil {
					log.Errorf("error sending event: %v", err)
					f.handleAsyncError(err)
				}
			case err := <-errChan:
				log.Errorf("error in spend subscription: %v", err)
				f.handleAsyncError(err)
			}
		}()
	})

	return nil
}

func (f *FSM) subscribeHyperloopNotifications() error {
	f.isConnectedMutex.Lock()
	defer f.isConnectedMutex.Unlock()
	if f.isConnectedToNotificationStream {
		return nil
	}

	ctx, cancel := context.WithCancel(f.ctx)

	// Subscribe to the notification stream.
	client, err := f.cfg.HyperloopClient.HyperloopNotificationStream(
		ctx, &swapserverrpc.HyperloopNotificationStreamRequest{
			HyperloopId: f.hyperloop.ID[:],
		},
	)
	if err != nil {
		f.Errorf("unable to subscribe to hyperloop notifications: %v", err)
		return err
	}
	f.Debugf("subscribed to hyperloop notifications")

	f.isConnectedToNotificationStream = true
	go func() {
		for {
			notification, err := client.Recv()
			if err == nil && notification != nil {
				f.lastNotification = notification
				continue
			}
			f.Errorf("error receiving hyperloop notification: %v", err)
			cancel()
			f.isConnectedMutex.Lock()
			f.isConnectedToNotificationStream = false
			f.isConnectedMutex.Unlock()

			// If we encounter an error, we'll try to reconnect.
			for {
				select {
				case <-f.ctx.Done():
					return

				case <-time.After(time.Second * 10):
					f.Debugf("reconnecting to hyperloop notifications")
					err = f.subscribeHyperloopNotifications()
					if err != nil {
						f.Errorf("error reconnecting: %v", err)
						continue
					}
					return
				}
			}
		}
	}()

	return nil
}

// getHyperloopSpendingEvent returns the event that should be triggered when the
// hyperloop output is spent. It returns an error if the spending transaction is
// unexpected.
func (f *FSM) getHyperloopSpendingEvent(spendDetails *chainntnfs.SpendDetail,
) fsm.EventType {

	spendingTxHash := spendDetails.SpendingTx.TxHash()

	htlcTx, err := f.hyperloop.getHyperLoopHtlcTx(f.cfg.ChainParams)
	if err != nil {
		return f.HandleError(err)
	}
	htlcTxHash := htlcTx.TxHash()

	if bytes.Equal(spendingTxHash[:], htlcTxHash[:]) {
		return OnHtlcPublished
	}

	sweeplessSweepTx, err := f.hyperloop.getHyperLoopSweeplessSweepTx()
	if err != nil {
		return f.HandleError(err)
	}

	sweeplessSweepTxHash := sweeplessSweepTx.TxHash()

	if bytes.Equal(spendingTxHash[:], sweeplessSweepTxHash[:]) {
		return OnSweeplessSweepPublish
	}

	return f.HandleError(errors.New("unexpected tx spent"))
}

// Infof logs an info message with the reservation hash as prefix.
func (f *FSM) Infof(format string, args ...interface{}) {
	log.Infof(
		"Hyperloop %v: "+format,
		append(
			[]interface{}{f.hyperloop.ID},
			args...,
		)...,
	)
}

// Debugf logs a debug message with the reservation hash as prefix.
func (f *FSM) Debugf(format string, args ...interface{}) {
	log.Debugf(
		"Hyperloop %v: "+format,
		append(
			[]interface{}{f.hyperloop.ID},
			args...,
		)...,
	)
}

// Errorf logs an error message with the reservation hash as prefix.
func (f *FSM) Errorf(format string, args ...interface{}) {
	log.Errorf(
		"Hyperloop %v: "+format,
		append(
			[]interface{}{f.hyperloop.ID},
			args...,
		)...,
	)
}

// isFinalState returns true if the FSM is in a final state.
func isFinalState(state fsm.StateType) bool {
	return state == SweepConfirmed || state == Failed
}
