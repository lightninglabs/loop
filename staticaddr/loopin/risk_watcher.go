package loopin

import (
	"bytes"
	"context"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
)

// confirmationRiskUpdate is the normalized result of a server confirmation-risk
// notification.
type confirmationRiskUpdate struct {
	decision ConfirmationRiskDecision
	reason   string
}

// confirmationRiskWatcher normalizes static loop-in confirmation risk
// notifications and restores the durable decision timestamp.
type confirmationRiskWatcher struct {
	swapHash            lntypes.Hash
	store               StaticAddressLoopInStore
	notificationManager NotificationManager
	logWarnf            func(string, ...any)
}

// newConfirmationRiskWatcher creates a helper that handles confirmation-risk
// notification plumbing for a single static loop-in swap.
func newConfirmationRiskWatcher(cfg *Config, swapHash lntypes.Hash,
	warnf func(string, ...any)) *confirmationRiskWatcher {

	return &confirmationRiskWatcher{
		swapHash:            swapHash,
		store:               cfg.Store,
		notificationManager: cfg.NotificationManager,
		logWarnf:            warnf,
	}
}

// warnf logs through the FSM-scoped logger when one is available.
func (w *confirmationRiskWatcher) warnf(format string, args ...any) {
	if w.logWarnf != nil {
		w.logWarnf(format, args...)

		return
	}

	log.Warnf(format, args...)
}

// subscribe subscribes to accepted and rejected confirmation-risk notifications
// and emits normalized updates for the watcher's swap hash.
func (w *confirmationRiskWatcher) subscribe(ctx context.Context) (
	<-chan confirmationRiskUpdate, func()) {

	if w.notificationManager == nil {
		return nil, func() {}
	}

	notificationCtx, cancel := context.WithCancel(ctx)
	riskAcceptedChan := w.notificationManager.SubscribeStaticLoopInRiskAccepted(
		notificationCtx, w.swapHash,
	)
	riskRejectedChan := w.notificationManager.SubscribeStaticLoopInRiskRejected(
		notificationCtx, w.swapHash,
	)

	riskUpdates := make(chan confirmationRiskUpdate, 1)
	go func() {
		defer close(riskUpdates)

		for {
			select {
			case riskAccepted, ok := <-riskAcceptedChan:
				if !ok {
					riskAcceptedChan = nil
					continue
				}

				if riskAccepted == nil || !bytes.Equal(
					riskAccepted.SwapHash, w.swapHash[:],
				) {

					continue
				}

				update := confirmationRiskUpdate{
					decision: ConfirmationRiskDecisionAccepted,
					reason:   "risk accepted notification",
				}
				select {
				case riskUpdates <- update:
				case <-notificationCtx.Done():
					return
				}

			case riskRejected, ok := <-riskRejectedChan:
				if !ok {
					riskRejectedChan = nil
					continue
				}

				if riskRejected == nil || !bytes.Equal(
					riskRejected.SwapHash, w.swapHash[:],
				) {

					continue
				}

				update := confirmationRiskUpdate{
					decision: ConfirmationRiskDecisionRejected,
					reason:   "risk rejection",
				}
				select {
				case riskUpdates <- update:
				case <-notificationCtx.Done():
					return
				}

			case <-notificationCtx.Done():
				return
			}
		}
	}()

	return riskUpdates, cancel
}

// durableDecisionTime returns the durable decision timestamp, recording the
// decision first if the notification was replayed before it could be persisted.
// The bool is false when a configured store could not durably record or reload
// the decision.
func (w *confirmationRiskWatcher) durableDecisionTime(ctx context.Context,
	decision ConfirmationRiskDecision) (time.Time, bool) {

	now := time.Now()
	if w.store == nil {
		return now, true
	}

	storedLoopIn, err := w.store.GetLoopInByHash(ctx, w.swapHash)
	if err != nil {
		w.warnf("unable to reload persisted risk decision for swap %v: %v",
			w.swapHash, err)

		return time.Time{}, false
	}

	if storedLoopIn == nil {
		return time.Time{}, false
	}

	hasPersistedDecision :=
		storedLoopIn.ConfirmationRiskDecision == decision &&
			!storedLoopIn.ConfirmationRiskDecisionTime.IsZero()

	if !hasPersistedDecision {
		err = w.store.RecordStaticAddressRiskDecision(
			ctx, w.swapHash, decision,
		)
		if err != nil {
			w.warnf("unable to persist replayed risk decision for "+
				"swap %v: %v", w.swapHash, err)

			return time.Time{}, false
		}

		storedLoopIn, err = w.store.GetLoopInByHash(ctx, w.swapHash)
		if err != nil {
			w.warnf("unable to reload persisted risk decision for "+
				"swap %v: %v", w.swapHash, err)

			return time.Time{}, false
		}
		if storedLoopIn == nil ||
			storedLoopIn.ConfirmationRiskDecision != decision ||
			storedLoopIn.ConfirmationRiskDecisionTime.IsZero() {

			return time.Time{}, false
		}
	}

	return storedLoopIn.ConfirmationRiskDecisionTime, true
}

// decisionTime retains the best-effort behavior used for server notifications.
func (w *confirmationRiskWatcher) decisionTime(ctx context.Context,
	decision ConfirmationRiskDecision) time.Time {

	decisionTime, ok := w.durableDecisionTime(ctx, decision)
	if !ok {
		return time.Now()
	}

	return decisionTime
}
