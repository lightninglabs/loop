package hyperloop

import (
	"bytes"
	"context"
	"sync"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

const (
	// defaultObserverSize is the size of the fsm observer channel.
	defaultObserverSize = 15
)

// States represents the possible states of the hyperloop FSM.
var (
	// Init is the initial state of the hyperloop FSM.
	Init = fsm.StateType("Init")

	// Registering is the state where we try to pay the hyperloop invoice.
	Registering = fsm.StateType("Registering")

	// WaitForPublish is the state where we wait for the hyperloop output
	// to be published.
	WaitForPublish = fsm.StateType("WaitForPublish")

	// WaitForConfirmation is the state where we wait for the hyperloop
	// output to be confirmed.
	WaitForConfirmation = fsm.StateType("WaitForConfirmation")

	// PushHtlcNonce is the state where we push the htlc nonce to the
	// server.
	PushHtlcNonce = fsm.StateType("PushHtlcNonce")

	// WaitForReadyForHtlcSig is the event that is triggered when the server
	// is ready to receive the htlc sig.
	WaitForReadyForHtlcSig = fsm.StateType("WaitForReadyForHtlcSig")

	// PushHtlcSig is the state where we push the htlc sig to the server.
	PushHtlcSig = fsm.StateType("PushHtlcSig")

	// WaitForHtlcSig is the state where we wait for the final htlc sig.
	WaitForHtlcSig = fsm.StateType("WaitForHtlcSig")

	// PushPreimage is the state where we push the preimage and sweepless
	// sweep nonce to the server.
	PushPreimage = fsm.StateType("PushPreimage")

	// WaitForReadyForSweeplessSweepSig is the event that is triggered when
	// the server is ready to receive the sweepless sweep sig.
	WaitForReadyForSweeplessSweepSig = fsm.StateType("WaitForReadyForSweeplessSweepSig") // nolint: lll

	// PushSweeplessSweepSig is the state where we push the sweepless sweep
	// sig to the server.
	PushSweeplessSweepSig = fsm.StateType("PushSweeplessSweepSig")

	// WaitForSweepPublish is the state where we wait for the sweep
	// transaction to be published.
	WaitForSweepPublish = fsm.StateType("WaitForSweepPublish")

	// WaitForSweepConfirmation is the state where we wait for the sweep
	// transaction to be confirmed.
	WaitForSweepConfirmation = fsm.StateType("WaitForSweepConfirmation")

	// SweepConfirmed is a final state where the sweep transaction has been
	// confirmed.
	SweepConfirmed = fsm.StateType("SweepConfirmed")

	// WaitForHtlcPublishBlockheight is the state where we wait for the
	// htlc publish blockheight to be reached.
	WaitForHtlcPublishBlockheight = fsm.StateType("WaitForHtlcPublishBlockheight") // nolint: lll

	// PublishHtlc is the state where we publish the htlc transaction.
	PublishHtlc = fsm.StateType("PublishHtlc")

	// TODO: implement HTLC states
	// WaitForHtlcConfirmation is the state where we wait for the htlc
	// transaction to be confirmed.
	// WaitForHtlcConfirmation = fsm.StateType("WaitForHtlcConfirmation").

	// HtlcConfirmed is the state where the htlc transaction has been
	// confirmed.
	// HtlcConfirmed = fsm.StateType("HtlcConfirmed").

	// PublishHtlcSweep is the state where we publish the htlc sweep
	// transaction.
	// PublishHtlcSweep = fsm.StateType("PublishHtlcSweep").

	// WaitForHtlcSweepConfirmation is the state where we wait for the htlc
	// sweep transaction to be confirmed.
	// WaitForHtlcSweepConfirmation = fsm.StateType("WaitForHtlcSweepConfirmation").

	// HtlcSweepConfirmed is the state where the htlc sweep transaction has
	// beenconfirmed.
	// HtlcSweepConfirmed = fsm.StateType("HtlcSweepConfirmed").

	// Failed is a final state where the hyperloop has failed.
	Failed = fsm.StateType("Failed")
)

// Events represents the possible events that can be sent to the hyperloop FSM.
var (
	// OnStart is the event that is sent when the hyperloop FSM is started.
	OnStart = fsm.EventType("OnStart")

	// OnInit is the event is sent when the FSM is initialized.
	OnInit = fsm.EventType("OnInit")

	// OnRegistered is the event that is triggered when we have been
	// registered with the server.
	OnRegistered = fsm.EventType("OnRegistered")

	// OnPublished is the event that is triggered when the hyperloop output
	// has been published.
	OnPublished = fsm.EventType("OnPublished")

	// OnConfirmed is the event that is triggered when the hyperloop output
	// has been confirmed.
	OnConfirmed = fsm.EventType("OnConfirmed")

	// OnPushedHtlcNonce is the event that is triggered when the htlc nonce
	// has been pushed to the server.
	OnPushedHtlcNonce = fsm.EventType("OnPushedHtlcNonce")

	// OnReadyForHtlcSig is the event that is triggered when the server is
	// ready to receive the htlc sig.
	OnReadyForHtlcSig = fsm.EventType("OnReadyForHtlcSig")

	// OnPushedHtlcSig is the event that is sent when the htlc sig has been
	// pushed to the server.
	OnPushedHtlcSig = fsm.EventType("OnPushedHtlcSig")

	// OnReceivedHtlcSig is the event that is sent when the htlc sig has
	// been received.
	OnReceivedHtlcSig = fsm.EventType("OnReceivedHtlcSig")

	// OnPushedPreimage is the event that is sent when the preimage has been
	// pushed to the server.
	OnPushedPreimage = fsm.EventType("OnPushedPreimage")

	// OnReadyForSweeplessSweepSig is the event that is sent when the server
	// is ready to receive the sweepless sweep sig.
	OnReadyForSweeplessSweepSig = fsm.EventType("OnReadyForSweeplessSweepSig")

	// OnPushedSweeplessSweepSig is the event that is sent when the
	// sweepless sweep sig has been pushed to the server.
	OnPushedSweeplessSweepSig = fsm.EventType("OnPushedSweeplessSweepSig")

	// OnSweeplessSweepPublish is the event that is sent when the sweepless
	// sweep transaction has been published.
	OnSweeplessSweepPublish = fsm.EventType("OnSweeplessSweepPublish")

	// OnSweeplessSweepConfirmed is the event that is sent when the
	// sweepless sweep transaction has been confirmed.
	OnSweeplessSweepConfirmed = fsm.EventType("OnSweeplessSweepConfirmed")

	// TODO: implement HTLC events
	// OnPublishHtlc is the event that is sent when we should publish the htlc
	// transaction.
	// OnPublishHtlc = fsm.EventType("OnPublishHtlc").

	// OnHtlcPublished is the event that is sent when the htlc transaction has
	// been published.
	// OnHtlcPublished = fsm.EventType("OnHtlcPublished").

	// OnHtlcConfirmed is the event that is sent when the htlc transaction has
	// been confirmed.
	// OnHtlcConfirmed = fsm.EventType("OnHtlcConfirmed").

	// OnSweepHtlc is the event that is sent when we publish the htlc sweep
	// transaction.
	// OnSweepHtlc = fsm.EventType("OnSweepHtlc").

	// OnSweepPublished is the event that is sent when the sweep transaction
	// has been published.
	OnSweepPublished = fsm.EventType("OnSweepPublished")

	// OnHtlcSweepConfirmed is the event that is sent when the htlc sweep
	// transaction has been confirmed.
	OnHtlcSweepConfirmed = fsm.EventType("OnHtlcSweepConfirmed")

	// OnHyperlppSpent is the event that is sent when the hyperloop output
	// has been spent.
	OnHyperloopSpent = fsm.EventType("OnHyperloopSpent")

	// OnPaymentFailed is the event that is sent when the payment has
	// failed.
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
	*fsm.GenericFSM[Hyperloop]

	cfg *Config

	swapAmtFetcher HyperloopManager

	// hyperloop contains all the data required for the hyperloop process.
	hyperloop *Hyperloop

	notificationManager *utils.SubscriptionManager[*swapserverrpc.HyperloopNotificationStreamResponse] // nolint: lll

	spendManager *utils.SubscriptionManager[*chainntnfs.SpendDetail]

	// lastNotification is the last notification that we received.
	lastNotification *swapserverrpc.HyperloopNotificationStreamResponse

	// lastNotificationMutex is used to ensure that we safely access the
	// lastNotification variable.
	lastNotificationMutex sync.Mutex

	// spendChan is the channel that we receive the spend notification on.
	spendChan chan *chainntnfs.SpendDetail
}

// NewFSM creates a new instant out FSM.
func NewFSM(cfg *Config, swapAmtFetcher HyperloopManager) (*FSM, error) {
	hyperloop := &Hyperloop{
		State: fsm.EmptyState,
	}

	return NewFSMFromHyperloop(cfg, hyperloop, swapAmtFetcher)
}

// NewFSMFromHyperloop creates a new instantout FSM from an existing instantout
// recovered from the database.
func NewFSMFromHyperloop(cfg *Config, hyperloop *Hyperloop,
	swapAmtFetcher HyperloopManager) (*FSM, error) {

	instantOutFSM := &FSM{
		cfg:            cfg,
		hyperloop:      hyperloop,
		spendChan:      make(chan *chainntnfs.SpendDetail),
		swapAmtFetcher: swapAmtFetcher,
	}

	stateMachine := fsm.NewStateMachineWithState(
		instantOutFSM.GetStateMap(), hyperloop.State,
		defaultObserverSize,
	)

	instantOutFSM.GenericFSM = fsm.NewGenericFSM(stateMachine, hyperloop)

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
		// At this point we have pushed the htlc sig to the server and
		// the hyperloop could be spent to the htlc at any time, so
		// we'll need to be careful and be ready to sweep it.
		WaitForHtlcSig: fsm.State{
			Transitions: fsm.Transitions{
				OnReceivedHtlcSig: PushPreimage,
				// TODO: implement htlc states
				// OnHtlcPublished:   WaitForHtlcConfirmation,
				fsm.OnError: Failed,
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
				// TODO: implement htlc states
				// OnHtlcPublished:             WaitForHtlcConfirmation,
				fsm.OnError: Failed,
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
				// TODO: implement htlc states
				// OnHtlcPublished:         WaitForHtlcConfirmation,
				fsm.OnError: Failed,
			},
			Action: f.waitForSweepPublishAction,
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
func (r *FSM) updateHyperloop(ctx context.Context,
	notification fsm.Notification) {

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

	// TODO: implement store.
	// err := r.cfg.Store.UpdateHyperloop(ctx, r.hyperloop)
	// if err != nil {
	// 	r.Errorf("unable to update hyperloop: %v", err)
	// }

	// Subscribe to the hyperloop notifications.
	r.subscribeHyperloopNotifications(ctx)

	// Subscribe to the spend notification of the current outpoint.
	r.subscribeHyperloopOutpointSpend(ctx)
}

func (f *FSM) subscribeHyperloopOutpointSpend(ctx context.Context) {
	if isFinalState(f.hyperloop.State) {
		// If we are in a final state, we stop the spend manager.
		if f.spendManager != nil && f.spendManager.IsSubscribed() {
			f.spendManager.Stop()
		}
		return
	}
	// If we don't have an outpoint yet, we can't subscribe to it.
	if f.hyperloop.ConfirmedOutpoint == nil {
		return
	}

	if f.spendManager == nil {
		subscription := &HyperloopOutpointSpendSubscription{fsm: f}
		f.spendManager = utils.NewSubscriptionManager(subscription)
	}

	if !f.spendManager.IsSubscribed() {
		f.spendManager.Start(ctx)
	}
}

type HyperloopOutpointSpendSubscription struct {
	fsm *FSM
}

func (hoss *HyperloopOutpointSpendSubscription) Subscribe(ctx context.Context,
) (<-chan *chainntnfs.SpendDetail, <-chan error, error) {

	pkscript, err := hoss.fsm.hyperloop.getHyperLoopScript()
	if err != nil {
		return nil, nil, err
	}

	spendChan, errChan, err := hoss.fsm.cfg.ChainNotifier.RegisterSpendNtfn(
		ctx, hoss.fsm.hyperloop.ConfirmedOutpoint, pkscript,
		hoss.fsm.hyperloop.ConfirmationHeight,
	)
	if err != nil {
		return nil, nil, err
	}

	// No need to wrap channels, just return them directly
	return spendChan, errChan, nil
}

func (hoss *HyperloopOutpointSpendSubscription) HandleEvent(
	event *chainntnfs.SpendDetail) error {

	hoss.fsm.spendChan <- event
	return nil
}

func (hoss *HyperloopOutpointSpendSubscription) HandleError(err error) {
	hoss.fsm.Errorf("error in spend subscription: %v", err)
	hoss.fsm.handleAsyncError(context.Background(), err)
}

func (f *FSM) subscribeHyperloopNotifications(ctx context.Context) {
	emptyId := ID{}
	if bytes.Equal(f.hyperloop.ID[:], emptyId[:]) {
		log.Errorf("hyperloop id is empty, can't subscribe to notifications")
		return
	}

	if isFinalState(f.hyperloop.State) {
		// If we are in a final state, we stop the ntfn manager.
		if f.notificationManager != nil &&
			f.notificationManager.IsSubscribed() {

			f.notificationManager.Stop()
		}
		return
	}
	if f.notificationManager == nil {
		subscription := &HyperloopNotificationSubscription{fsm: f}
		f.notificationManager = utils.NewSubscriptionManager(
			subscription,
		)
	}

	if !f.notificationManager.IsSubscribed() {
		f.notificationManager.Start(ctx)
	}
}

type HyperloopNotificationSubscription struct {
	fsm *FSM
}

func (hns *HyperloopNotificationSubscription) Subscribe(ctx context.Context) (
	<-chan *swapserverrpc.HyperloopNotificationStreamResponse, <-chan error,
	error) {

	client, err := hns.fsm.cfg.HyperloopClient.HyperloopNotificationStream(
		ctx, &swapserverrpc.HyperloopNotificationStreamRequest{
			HyperloopId: hns.fsm.hyperloop.ID[:],
		},
	)
	if err != nil {
		return nil, nil, err
	}

	eventChan := make(
		chan *swapserverrpc.HyperloopNotificationStreamResponse,
	)
	errChan := make(chan error)

	go func() {
		defer close(eventChan)
		defer close(errChan)
		for {
			notification, err := client.Recv()
			if err != nil {
				hns.fsm.Errorf("error receiving hyperloop notification: %v", err)
				errChan <- err
				return
			}
			hns.fsm.Debugf("Received notification: %v", notification)
			eventChan <- notification
		}
	}()

	return eventChan, errChan, nil
}

func (hns *HyperloopNotificationSubscription) HandleEvent(
	event *swapserverrpc.HyperloopNotificationStreamResponse) error {

	hns.fsm.lastNotificationMutex.Lock()
	hns.fsm.lastNotification = event
	hns.fsm.lastNotificationMutex.Unlock()
	return nil
}

func (hns *HyperloopNotificationSubscription) HandleError(err error) {
	hns.fsm.Errorf("error receiving hyperloop notification: %v", err)
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
			[]interface{}{f.hyperloop.String()},
			args...,
		)...,
	)
}

// Errorf logs an error message with the reservation hash as prefix.
func (f *FSM) Errorf(format string, args ...interface{}) {
	log.Errorf(
		"Hyperloop %v: "+format,
		append(
			[]interface{}{f.hyperloop.String()},
			args...,
		)...,
	)
}

// isFinalState returns true if the FSM is in a final state.
func isFinalState(state fsm.StateType) bool {
	return state == SweepConfirmed || state == Failed
}
