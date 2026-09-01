package server

import (
	"context"
	"sync"
	"time"

	"github.com/lightninglabs/loop/swapserverrpc"
)

type serverUpdate struct {
	state     swapserverrpc.ServerSwapState
	timestamp time.Time
}

type updateSubscription struct {
	history []serverUpdate
	updates <-chan serverUpdate
	done    bool
	cancel  func()
}

type updateHub struct {
	mu          sync.Mutex
	history     []serverUpdate
	subscribers map[uint64]chan serverUpdate
	nextID      uint64
	done        bool
}

func newUpdateHub() *updateHub {
	return &updateHub{
		subscribers: make(map[uint64]chan serverUpdate),
	}
}

func (h *updateHub) publish(state swapserverrpc.ServerSwapState) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.done {
		return
	}

	update := serverUpdate{
		state:     state,
		timestamp: time.Now(),
	}
	h.history = append(h.history, update)

	for _, subscriber := range h.subscribers {
		select {
		case subscriber <- update:
		default:
		}
	}
}

func (h *updateHub) finish(state swapserverrpc.ServerSwapState) {
	h.publish(state)

	h.mu.Lock()
	h.done = true
	for id, subscriber := range h.subscribers {
		close(subscriber)
		delete(h.subscribers, id)
	}
	h.mu.Unlock()
}

func (h *updateHub) subscribe() updateSubscription {
	h.mu.Lock()
	defer h.mu.Unlock()

	history := append([]serverUpdate(nil), h.history...)
	if h.done {
		return updateSubscription{
			history: history,
			done:    true,
			cancel:  func() {},
		}
	}

	id := h.nextID
	h.nextID++
	updates := make(chan serverUpdate, 16)
	h.subscribers[id] = updates

	var once sync.Once
	return updateSubscription{
		history: history,
		updates: updates,
		cancel: func() {
			once.Do(func() {
				h.mu.Lock()
				if subscriber, ok := h.subscribers[id]; ok {
					delete(h.subscribers, id)
					close(subscriber)
				}
				h.mu.Unlock()
			})
		},
	}
}

type notificationHub struct {
	mu          sync.Mutex
	history     []*swapserverrpc.SubscribeNotificationsResponse
	subscribers map[uint64]chan *swapserverrpc.SubscribeNotificationsResponse
	nextID      uint64
}

const notificationHistoryLimit = 128

func newNotificationHub() *notificationHub {
	return &notificationHub{
		subscribers: make(
			map[uint64]chan *swapserverrpc.SubscribeNotificationsResponse,
		),
	}
}

func (h *notificationHub) publish(
	notification *swapserverrpc.SubscribeNotificationsResponse) {

	h.mu.Lock()
	defer h.mu.Unlock()

	h.history = append(h.history, notification)
	if len(h.history) > notificationHistoryLimit {
		h.history = append(
			[]*swapserverrpc.SubscribeNotificationsResponse(nil),
			h.history[len(h.history)-notificationHistoryLimit:]...,
		)
	}

	for _, subscriber := range h.subscribers {
		select {
		case subscriber <- notification:
		default:
		}
	}
}

func (h *notificationHub) subscribe(
	ctx context.Context) <-chan *swapserverrpc.SubscribeNotificationsResponse {

	h.mu.Lock()
	id := h.nextID
	h.nextID++
	updates := make(
		chan *swapserverrpc.SubscribeNotificationsResponse,
		len(h.history)+16,
	)
	for _, notification := range h.history {
		updates <- notification
	}
	h.subscribers[id] = updates
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		h.mu.Lock()
		if subscriber, ok := h.subscribers[id]; ok {
			delete(h.subscribers, id)
			close(subscriber)
		}
		h.mu.Unlock()
	}()

	return updates
}
