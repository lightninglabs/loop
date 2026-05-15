package notifications

import (
	"context"
	"sync"
	"time"

	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
)

// NotificationType is the type of notification that the manager can handle.
type NotificationType int

const (
	// NotificationTypeUnknown is the default notification type.
	NotificationTypeUnknown NotificationType = iota

	// NotificationTypeReservation is the notification type for reservation
	// notifications.
	NotificationTypeReservation

	// NotificationTypeStaticLoopInSweepRequest is the notification type for
	// static loop in sweep requests.
	NotificationTypeStaticLoopInSweepRequest

	// NotificationTypeStaticLoopInRiskAccepted is the notification type for
	// static loop in confirmation risk acceptance.
	NotificationTypeStaticLoopInRiskAccepted

	// NotificationTypeStaticLoopInRiskRejected is the notification type for
	// static loop in confirmation risk rejection.
	NotificationTypeStaticLoopInRiskRejected

	// NotificationTypeUnfinishedSwap is the notification type for unfinished
	// swap notifications.
	NotificationTypeUnfinishedSwap
)

const (
	// defaultMinAliveConnTime is the default minimum time that the
	// connection to the server needs to be alive before we consider it a
	// successful connection.
	defaultMinAliveConnTime = time.Minute

	// current_version is the current version of the notification listener.
	current_version = swapserverrpc.SubscribeNotificationsRequest_V1
)

// Client is the interface that the notification manager needs to implement in
// order to be able to subscribe to notifications.
type Client interface {
	// SubscribeNotifications subscribes to the notifications from the server.
	SubscribeNotifications(ctx context.Context,
		in *swapserverrpc.SubscribeNotificationsRequest,
		opts ...grpc.CallOption) (
		swapserverrpc.SwapServer_SubscribeNotificationsClient, error)
}

// Config contains all the services that the notification manager needs to
// operate.
type Config struct {
	// Client is the client used to communicate with the swap server.
	Client Client

	// CurrentToken returns the token that is currently contained in the
	// store or an l402.ErrNoToken error if there is none.
	CurrentToken func() (*l402.Token, error)

	// MinAliveConnTime is the minimum time that the connection to the
	// server needs to be alive before we consider it a successful.
	MinAliveConnTime time.Duration

	// PersistStaticLoopInRiskDecision durably records static loop-in
	// confirmation-risk decisions before they are forwarded to subscribers.
	PersistStaticLoopInRiskDecision func(context.Context, lntypes.Hash,
		bool) error
}

// Manager is a manager for notifications that the swap server sends to the
// client.
type Manager struct {
	sync.Mutex

	cfg *Config

	hasL402 bool

	subscribers map[NotificationType][]subscriber

	staticLoopInRiskAccepted map[lntypes.Hash]*swapserverrpc.
					ServerStaticLoopInRiskAcceptedNotification

	staticLoopInRiskRejected map[lntypes.Hash]*swapserverrpc.
					ServerStaticLoopInRiskRejectedNotification
}

// NewManager creates a new notification manager.
func NewManager(cfg *Config) *Manager {
	// Set the default minimum alive connection time if it's not set.
	if cfg.MinAliveConnTime == 0 {
		cfg.MinAliveConnTime = defaultMinAliveConnTime
	}

	return &Manager{
		cfg:         cfg,
		subscribers: make(map[NotificationType][]subscriber),
		staticLoopInRiskAccepted: make(
			map[lntypes.Hash]*swapserverrpc.
				ServerStaticLoopInRiskAcceptedNotification,
		),
		staticLoopInRiskRejected: make(
			map[lntypes.Hash]*swapserverrpc.
				ServerStaticLoopInRiskRejectedNotification,
		),
	}
}

type subscriber struct {
	subCtx   context.Context
	recvChan any
	swapHash *lntypes.Hash
	enqueue  func(any)
}

func newNotificationQueue[T any](ctx context.Context,
	recvChan chan T) func(any) {

	type queue struct {
		sync.Mutex

		pending []T
		notify  chan struct{}
	}

	q := &queue{
		notify: make(chan struct{}, 1),
	}

	go func() {
		defer func() {
			if recover() != nil {
				log.Debugf("subscriber channel closed before " +
					"notification delivery")
			}
		}()

		for {
			q.Lock()
			if len(q.pending) == 0 {
				q.Unlock()

				select {
				case <-q.notify:
					continue

				case <-ctx.Done():
					return
				}
			}

			ntfn := q.pending[0]
			q.pending = q.pending[1:]
			q.Unlock()

			select {
			case recvChan <- ntfn:
			case <-ctx.Done():
				return
			}
		}
	}()

	return func(ntfn any) {
		typedNtfn, ok := ntfn.(T)
		if !ok {
			log.Warnf("unexpected notification type %T", ntfn)
			return
		}

		q.Lock()
		q.pending = append(q.pending, typedNtfn)
		q.Unlock()

		select {
		case q.notify <- struct{}{}:
		default:
		}
	}
}

func queueNotification[T any](sub subscriber, recvChan chan T, ntfn T) {
	if sub.enqueue != nil {
		sub.enqueue(ntfn)
		return
	}

	select {
	case recvChan <- ntfn:
	case <-sub.subCtx.Done():
	}
}

// SubscribeReservations subscribes to the reservation notifications.
func (m *Manager) SubscribeReservations(ctx context.Context,
) <-chan *swapserverrpc.ServerReservationNotification {

	notifChan := make(chan *swapserverrpc.ServerReservationNotification, 1)
	sub := subscriber{
		subCtx:   ctx,
		recvChan: notifChan,
	}

	m.addSubscriber(NotificationTypeReservation, sub)

	context.AfterFunc(ctx, func() {
		m.removeSubscriber(NotificationTypeReservation, sub)
		close(notifChan)
	})

	return notifChan
}

// SubscribeStaticLoopInSweepRequests subscribes to the static loop in sweep
// requests.
func (m *Manager) SubscribeStaticLoopInSweepRequests(ctx context.Context,
) <-chan *swapserverrpc.ServerStaticLoopInSweepNotification {

	notifChan := make(
		chan *swapserverrpc.ServerStaticLoopInSweepNotification, 1,
	)

	sub := subscriber{
		subCtx:   ctx,
		recvChan: notifChan,
		enqueue:  newNotificationQueue(ctx, notifChan),
	}

	m.addSubscriber(NotificationTypeStaticLoopInSweepRequest, sub)

	context.AfterFunc(ctx, func() {
		m.removeSubscriber(
			NotificationTypeStaticLoopInSweepRequest,
			sub,
		)
		close(notifChan)
	})

	return notifChan
}

// SubscribeStaticLoopInRiskAccepted subscribes to static loop in risk accepted
// notifications.
func (m *Manager) SubscribeStaticLoopInRiskAccepted(ctx context.Context,
	swapHash lntypes.Hash,
) <-chan *swapserverrpc.ServerStaticLoopInRiskAcceptedNotification {

	notifChan := make(
		chan *swapserverrpc.ServerStaticLoopInRiskAcceptedNotification, 1,
	)

	sub := subscriber{
		subCtx:   ctx,
		recvChan: notifChan,
		swapHash: &swapHash,
	}

	m.Lock()
	m.subscribers[NotificationTypeStaticLoopInRiskAccepted] = append(
		m.subscribers[NotificationTypeStaticLoopInRiskAccepted], sub,
	)
	if ntfn, ok := m.staticLoopInRiskAccepted[swapHash]; ok {
		notifChan <- ntfn
		delete(m.staticLoopInRiskAccepted, swapHash)
	}
	m.Unlock()

	context.AfterFunc(ctx, func() {
		m.removeSubscriber(NotificationTypeStaticLoopInRiskAccepted, sub)
		m.Lock()
		delete(m.staticLoopInRiskAccepted, swapHash)
		m.Unlock()
		close(notifChan)
	})

	return notifChan
}

// SubscribeStaticLoopInRiskRejected subscribes to static loop in risk rejected
// notifications.
func (m *Manager) SubscribeStaticLoopInRiskRejected(ctx context.Context,
	swapHash lntypes.Hash,
) <-chan *swapserverrpc.ServerStaticLoopInRiskRejectedNotification {

	notifChan := make(
		chan *swapserverrpc.ServerStaticLoopInRiskRejectedNotification, 1,
	)

	sub := subscriber{
		subCtx:   ctx,
		recvChan: notifChan,
		swapHash: &swapHash,
	}

	m.Lock()
	m.subscribers[NotificationTypeStaticLoopInRiskRejected] = append(
		m.subscribers[NotificationTypeStaticLoopInRiskRejected], sub,
	)
	if ntfn, ok := m.staticLoopInRiskRejected[swapHash]; ok {
		notifChan <- ntfn
		delete(m.staticLoopInRiskRejected, swapHash)
	}
	m.Unlock()

	context.AfterFunc(ctx, func() {
		m.removeSubscriber(NotificationTypeStaticLoopInRiskRejected, sub)
		m.Lock()
		delete(m.staticLoopInRiskRejected, swapHash)
		m.Unlock()
		close(notifChan)
	})

	return notifChan
}

// SubscribeUnfinishedSwaps subscribes to the unfinished swap notifications.
func (m *Manager) SubscribeUnfinishedSwaps(ctx context.Context,
) <-chan *swapserverrpc.ServerUnfinishedSwapNotification {

	notifChan := make(
		chan *swapserverrpc.ServerUnfinishedSwapNotification, 1,
	)
	sub := subscriber{
		subCtx:   ctx,
		recvChan: notifChan,
		enqueue:  newNotificationQueue(ctx, notifChan),
	}

	m.addSubscriber(NotificationTypeUnfinishedSwap, sub)
	context.AfterFunc(ctx, func() {
		m.removeSubscriber(NotificationTypeUnfinishedSwap, sub)
		close(notifChan)
	})

	return notifChan
}

// Run starts the notification manager. It will keep on running until the
// context is canceled. It will subscribe to notifications and forward them to
// the subscribers. On a first successful connection to the server, it will
// close the readyChan to signal that the manager is ready.
func (m *Manager) Run(ctx context.Context) error {
	// Initially we want to immediately try to connect to the server.
	var (
		waitTime time.Duration
		backoff  time.Duration
		attempts int
		timer    = time.NewTimer(0)
	)

	// Start the notification runloop.
	for {
		// Increase the wait time for the next iteration.
		backoff = waitTime + time.Duration(attempts)*time.Second
		waitTime = 0

		// Reset the timer with the new backoff time.
		timer.Reset(backoff)

		// Return if the context has been canceled.
		select {
		case <-ctx.Done():
			return nil

		case <-timer.C:
		}

		// In order to create a valid l402 we first are going to call
		// the FetchL402 method. As a client might not have outbound
		// capacity yet, we'll retry until we get a valid response.
		if !m.hasL402 {
			token, err := m.cfg.CurrentToken()
			if err != nil {
				// We only log the error if it's not the case
				// that we don't have a token yet to avoid
				// spamming the logs.
				if err != l402.ErrNoToken {
					log.Errorf("Error getting L402 from "+
						"the store: %v", err)
				}

				// Use a default of 1 second wait time to avoid
				// hogging the CPU.
				waitTime = time.Second
				continue
			}

			// If the preimage is empty, we don't have a valid L402
			// yet so we'll continue to retry with the incremental
			// backoff.
			emptyPreimage := lntypes.Preimage{}
			if token.Preimage == emptyPreimage {
				attempts++
				continue
			}

			attempts = 0
			m.hasL402 = true
		}

		connectAttempted := time.Now()
		err := m.subscribeNotifications(ctx)
		if err != nil {
			log.Errorf("Error subscribing to notifications: %v",
				err)
		}
		connectionAliveTime := time.Since(connectAttempted)

		// Note that we may be able to connet to the stream but not
		// able to use it if the client is unable to pay for their
		// L402. In this case the subscription will fail on the first
		// read immediately after connecting. We'll therefore only
		// consider the connection successful if we were able to use
		// the stream for at least the minimum alive connection time
		// (which defaults to 1 minute).
		if connectionAliveTime > m.cfg.MinAliveConnTime {
			// Reset the backoff to 10 seconds and the connect
			// attempts to zero if we were really connected for a
			// considerable amount of time (1 minute).
			waitTime = time.Second * 10
			attempts = 0
		} else {
			// We either failed to connect or the stream
			// disconnected immediately, so we just increase the
			// backoff.
			attempts++
		}
	}
}

// subscribeNotifications subscribes to the notifications from the server.
func (m *Manager) subscribeNotifications(ctx context.Context) error {
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	notifStream, err := m.cfg.Client.SubscribeNotifications(
		callCtx, &swapserverrpc.SubscribeNotificationsRequest{
			Version: current_version,
		},
	)
	if err != nil {
		return err
	}

	log.Debugf("Successfully subscribed to server notifications")

	for {
		notification, err := notifStream.Recv()
		if err == nil && notification != nil {
			log.Tracef("Received notification: %v", notification)
			m.handleNotification(ctx, notification)
			continue
		}

		log.Errorf("Error receiving notification: %v", err)

		return err
	}
}

// handleNotification handles an incoming notification from the server,
// forwarding it to the appropriate subscribers.
func (m *Manager) handleNotification(ctx context.Context, ntfn *swapserverrpc.
	SubscribeNotificationsResponse) {

	switch ntfn.Notification.(type) {
	case *swapserverrpc.SubscribeNotificationsResponse_ReservationNotification: // nolint: lll
		// We'll forward the reservation notification to all subscribers.
		reservationNtfn := ntfn.GetReservationNotification()
		m.Lock()
		defer m.Unlock()

		for _, sub := range m.subscribers[NotificationTypeReservation] {
			recvChan := sub.recvChan.(chan *swapserverrpc.
				ServerReservationNotification)

			select {
			case recvChan <- reservationNtfn:
			case <-sub.subCtx.Done():
			default:
				log.Debugf("Dropping reservation " +
					"notification for slow subscriber")
			}
		}
	case *swapserverrpc.SubscribeNotificationsResponse_StaticLoopInSweep: // nolint: lll
		// We'll forward the static loop in sweep request to all
		// subscribers.
		staticLoopInSweepRequestNtfn := ntfn.GetStaticLoopInSweep()
		m.Lock()
		defer m.Unlock()

		for _, sub := range m.subscribers[NotificationTypeStaticLoopInSweepRequest] { // nolint: lll
			recvChan := sub.recvChan.(chan *swapserverrpc.
				ServerStaticLoopInSweepNotification)

			queueNotification(sub, recvChan, staticLoopInSweepRequestNtfn)
		}

	case *swapserverrpc.SubscribeNotificationsResponse_StaticLoopInRiskAccepted: // nolint: lll
		// We'll forward the static loop in risk accepted notification to the
		// subscriber for the matching swap.
		riskAcceptedNtfn := ntfn.GetStaticLoopInRiskAccepted()
		var (
			swapHash    lntypes.Hash
			hasSwapHash bool
		)
		if riskAcceptedNtfn != nil {
			hash, err := lntypes.MakeHash(riskAcceptedNtfn.SwapHash)
			if err != nil {
				log.Warnf("Received invalid static loop in risk "+
					"accepted notification: %v", err)
			} else {
				swapHash = hash
				hasSwapHash = true
			}
		}

		if hasSwapHash && m.cfg.PersistStaticLoopInRiskDecision != nil {
			err := m.cfg.PersistStaticLoopInRiskDecision(
				ctx, swapHash, true,
			)
			if err != nil {
				log.Errorf("Unable to persist static loop in "+
					"risk accepted notification: %v", err)
				return
			}
		}

		m.Lock()
		defer m.Unlock()

		if hasSwapHash {
			m.staticLoopInRiskAccepted[swapHash] =
				riskAcceptedNtfn
			delete(m.staticLoopInRiskRejected, swapHash)
		}

		for _, sub := range m.subscribers[NotificationTypeStaticLoopInRiskAccepted] { // nolint: lll
			if !hasSwapHash || sub.swapHash == nil ||
				*sub.swapHash != swapHash {

				continue
			}

			recvChan := sub.recvChan.(chan *swapserverrpc.
				ServerStaticLoopInRiskAcceptedNotification)

			select {
			case recvChan <- riskAcceptedNtfn:
			case <-sub.subCtx.Done():
			default:
				log.Debugf("Dropping static loop in risk " +
					"accepted notification for slow subscriber")
			}
		}

	case *swapserverrpc.SubscribeNotificationsResponse_StaticLoopInRiskRejected: // nolint: lll
		// We'll forward the static loop in risk rejected notification to the
		// subscriber for the matching swap.
		riskRejectedNtfn := ntfn.GetStaticLoopInRiskRejected()
		var (
			swapHash    lntypes.Hash
			hasSwapHash bool
		)
		if riskRejectedNtfn != nil {
			hash, err := lntypes.MakeHash(riskRejectedNtfn.SwapHash)
			if err != nil {
				log.Warnf("Received invalid static loop in risk "+
					"rejected notification: %v", err)
			} else {
				swapHash = hash
				hasSwapHash = true
			}
		}

		if hasSwapHash && m.cfg.PersistStaticLoopInRiskDecision != nil {
			err := m.cfg.PersistStaticLoopInRiskDecision(
				ctx, swapHash, false,
			)
			if err != nil {
				log.Errorf("Unable to persist static loop in "+
					"risk rejected notification: %v", err)
				return
			}
		}

		m.Lock()
		defer m.Unlock()

		if hasSwapHash {
			m.staticLoopInRiskRejected[swapHash] =
				riskRejectedNtfn
			delete(m.staticLoopInRiskAccepted, swapHash)
		}

		for _, sub := range m.subscribers[NotificationTypeStaticLoopInRiskRejected] { // nolint: lll
			if !hasSwapHash || sub.swapHash == nil ||
				*sub.swapHash != swapHash {

				continue
			}

			recvChan := sub.recvChan.(chan *swapserverrpc.
				ServerStaticLoopInRiskRejectedNotification)

			select {
			case recvChan <- riskRejectedNtfn:
			case <-sub.subCtx.Done():
			default:
				log.Debugf("Dropping static loop in risk " +
					"rejected notification for slow subscriber")
			}
		}

	case *swapserverrpc.SubscribeNotificationsResponse_UnfinishedSwap: // nolint: lll
		// We'll forward the unfinished swap notification to all
		// subscribers.
		unfinishedSwapNtfn := ntfn.GetUnfinishedSwap()
		m.Lock()
		defer m.Unlock()

		for _, sub := range m.subscribers[NotificationTypeUnfinishedSwap] {
			recvChan := sub.recvChan.(chan *swapserverrpc.
				ServerUnfinishedSwapNotification)

			queueNotification(sub, recvChan, unfinishedSwapNtfn)
		}

	default:
		log.Warnf("Received unknown notification type: %v",
			ntfn)
	}
}

// addSubscriber adds a subscriber to the manager.
func (m *Manager) addSubscriber(notifType NotificationType, sub subscriber) {
	m.Lock()
	defer m.Unlock()
	m.subscribers[notifType] = append(m.subscribers[notifType], sub)
}

// removeSubscriber removes a subscriber from the manager.
func (m *Manager) removeSubscriber(notifType NotificationType, sub subscriber) {
	m.Lock()
	defer m.Unlock()
	subs := m.subscribers[notifType]
	newSubs := make([]subscriber, 0, len(subs))
	for _, s := range subs {
		if s.recvChan != sub.recvChan {
			newSubs = append(newSubs, s)
		}
	}
	m.subscribers[notifType] = newSubs
}
