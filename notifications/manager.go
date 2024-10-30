package notifications

import (
	"context"
	"sync"
	"time"

	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/loop/swapserverrpc"
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
}

// Manager is a manager for notifications that the swap server sends to the
// client.
type Manager struct {
	cfg *Config

	hasL402 bool

	subscribers map[NotificationType][]subscriber
	sync.Mutex
}

// NewManager creates a new notification manager.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:         cfg,
		subscribers: make(map[NotificationType][]subscriber),
	}
}

type subscriber struct {
	subCtx   context.Context
	recvChan interface{}
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

	// Start a goroutine to remove the subscriber when the context is canceled
	go func() {
		<-ctx.Done()
		m.removeSubscriber(NotificationTypeReservation, sub)
		close(notifChan)
	}()

	return notifChan
}

// Run starts the notification manager. It will keep on running until the
// context is canceled. It will subscribe to notifications and forward them to
// the subscribers. On a first successful connection to the server, it will
// close the readyChan to signal that the manager is ready.
func (m *Manager) Run(ctx context.Context) error {
	// Initially we want to immediately try to connect to the server.
	waitTime := time.Duration(0)

	// Start the notification runloop.
	for {
		timer := time.NewTimer(waitTime)
		// Increase the wait time for the next iteration.
		waitTime += time.Second * 1

		// Return if the context has been canceled.
		select {
		case <-ctx.Done():
			return nil

		case <-timer.C:
		}

		// In order to create a valid l402 we first are going to call
		// the FetchL402 method. As a client might not have outbound capacity
		// yet, we'll retry until we get a valid response.
		if !m.hasL402 {
			_, err := m.cfg.CurrentToken()
			if err != nil {
				// We only log the error if it's not the case that we
				// don't have a token yet to avoid spamming the logs.
				if err != l402.ErrNoToken {
					log.Errorf("Error getting L402 from store: %v", err)
				}
				continue
			}
			m.hasL402 = true
		}

		connectedFunc := func() {
			// Reset the wait time to 10 seconds.
			waitTime = time.Second * 10
		}

		err := m.subscribeNotifications(ctx, connectedFunc)
		if err != nil {
			log.Errorf("Error subscribing to notifications: %v", err)
		}
	}
}

// subscribeNotifications subscribes to the notifications from the server.
func (m *Manager) subscribeNotifications(ctx context.Context,
	connectedFunc func()) error {

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	notifStream, err := m.cfg.Client.SubscribeNotifications(
		callCtx, &swapserverrpc.SubscribeNotificationsRequest{},
	)
	if err != nil {
		return err
	}

	// Signal that we're connected to the server.
	connectedFunc()
	log.Debugf("Successfully subscribed to server notifications")

	for {
		notification, err := notifStream.Recv()
		if err == nil && notification != nil {
			log.Debugf("Received notification: %v", notification)
			m.handleNotification(notification)
			continue
		}

		log.Errorf("Error receiving notification: %v", err)

		return err
	}
}

// handleNotification handles an incoming notification from the server,
// forwarding it to the appropriate subscribers.
func (m *Manager) handleNotification(notification *swapserverrpc.
	SubscribeNotificationsResponse) {

	switch notification.Notification.(type) {
	case *swapserverrpc.SubscribeNotificationsResponse_ReservationNotification:
		// We'll forward the reservation notification to all subscribers.
		reservationNtfn := notification.GetReservationNotification()
		m.Lock()
		defer m.Unlock()

		for _, sub := range m.subscribers[NotificationTypeReservation] {
			recvChan := sub.recvChan.(chan *swapserverrpc.
				ServerReservationNotification)

			recvChan <- reservationNtfn
		}

	default:
		log.Warnf("Received unknown notification type: %v",
			notification)
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
		if s != sub {
			newSubs = append(newSubs, s)
		}
	}
	m.subscribers[notifType] = newSubs
}
