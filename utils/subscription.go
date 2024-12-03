package utils

import (
	"context"
	"sync"
	"time"
)

// Subscription is an interface that allows to subscribe to events and handle
// them.
type Subscription[E any] interface {
	Subscribe(ctx context.Context) (eventChan <-chan E, errChan <-chan error, err error)
	HandleEvent(event E) error
	HandleError(err error)
}

// SubscriptionManager is a manager that handles the subscription lifecycle.
type SubscriptionManager[E any] struct {
	subscription Subscription[E]
	isSubscribed bool
	mutex        sync.Mutex
	backoff      time.Duration
	quitChan     chan struct{}
}

// NewSubscriptionManager creates a new subscription manager.
func NewSubscriptionManager[E any](subscription Subscription[E],
) *SubscriptionManager[E] {

	return &SubscriptionManager[E]{
		subscription: subscription,
		backoff:      time.Second * 2,
		quitChan:     make(chan struct{}),
	}
}

// IsSubscribed returns true if the subscription manager is currently
// subscribed.
func (sm *SubscriptionManager[E]) IsSubscribed() bool {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	return sm.isSubscribed
}

// Start starts the subscription manager.
func (sm *SubscriptionManager[E]) Start(ctx context.Context) {
	sm.mutex.Lock()
	if sm.isSubscribed {
		sm.mutex.Unlock()
		return
	}
	sm.mutex.Unlock()

	go sm.manageSubscription(ctx)
}

// Stop stops the subscription manager.
func (sm *SubscriptionManager[E]) Stop() {
	close(sm.quitChan)
}

// manageSubscription manages the subscription lifecycle.
func (sm *SubscriptionManager[E]) manageSubscription(ctx context.Context) {
	defer func() {
		sm.mutex.Lock()
		sm.isSubscribed = false
		sm.mutex.Unlock()
	}()

	// The outer loop is used to retry the subscription. In case of an
	// error it will retry the subscription after a backoff, until the
	// context is done or the quit channel is closed.
	for {
		eventChan, errChan, err := sm.subscription.Subscribe(ctx)
		if err != nil {
			sm.subscription.HandleError(err)
			if !sm.shouldRetry(ctx) {
				return
			}
			continue
		}

		sm.mutex.Lock()
		sm.isSubscribed = true
		sm.mutex.Unlock()

		// The inner loop is used to handle events and errors. It will
		// retry the subscription in case of an error, until the context
		// is done or the quit channel is closed.
	handleLoop:
		for {
			select {
			case event, ok := <-eventChan:
				if !ok {
					if !sm.shouldRetry(ctx) {
						return
					}
					break handleLoop
				}
				if err := sm.subscription.HandleEvent(event); err != nil {
					sm.subscription.HandleError(err)
				}

			case err, ok := <-errChan:
				if !ok || err == nil {
					if !sm.shouldRetry(ctx) {
						return
					}
					break handleLoop
				}
				sm.subscription.HandleError(err)
				if !sm.shouldRetry(ctx) {
					return
				}
				// If we receive an error we break out of the
				// handleLoop to retry the subscription.
				break handleLoop

			case <-ctx.Done():
				return

			case <-sm.quitChan:
				return
			}
		}
	}
}

// shouldRetry determines if the subscription manager should retry the
// subscription.
func (sm *SubscriptionManager[E]) shouldRetry(ctx context.Context) bool {
	sm.mutex.Lock()
	sm.isSubscribed = false
	sm.mutex.Unlock()

	select {
	case <-sm.quitChan:
		return false
	case <-ctx.Done():
		return false
	case <-time.After(sm.backoff):
		// Exponential backoff with cap
		if sm.backoff < time.Minute {
			sm.backoff *= 2
		}
		return true
	}
}
