package fsm

import (
	"context"
	"sync"
	"time"
)

// CachedObserver is an observer that caches all states and transitions of
// the observed state machine.
type CachedObserver struct {
	lastNotification    Notification
	cachedNotifications *FixedSizeSlice[Notification]

	notificationCond *sync.Cond
	notificationMx   sync.Mutex
}

// NewCachedObserver creates a new cached observer with the given maximum
// number of cached notifications.
func NewCachedObserver(maxElements int) *CachedObserver {
	fixedSizeSlice := NewFixedSizeSlice[Notification](maxElements)
	observer := &CachedObserver{
		cachedNotifications: fixedSizeSlice,
	}
	observer.notificationCond = sync.NewCond(&observer.notificationMx)

	return observer
}

// Notify implements the Observer interface.
func (c *CachedObserver) Notify(notification Notification) {
	c.notificationMx.Lock()
	defer c.notificationMx.Unlock()

	c.cachedNotifications.Add(notification)
	c.lastNotification = notification
	c.notificationCond.Broadcast()
}

// GetCachedNotifications returns a copy of the  cached notifications.
func (c *CachedObserver) GetCachedNotifications() []Notification {
	c.notificationMx.Lock()
	defer c.notificationMx.Unlock()

	return c.cachedNotifications.Get()
}

// WaitForState waits for the state machine to reach the given state.
func (s *CachedObserver) WaitForState(ctx context.Context,
	timeout time.Duration, state StateType) error {

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Channel to notify when the desired state is reached
	ch := make(chan struct{})

	// Goroutine to wait on condition variable
	go func() {
		s.notificationMx.Lock()
		defer s.notificationMx.Unlock()

		for {
			// Check if the last state is the desired state
			if s.lastNotification.NextState == state {
				ch <- struct{}{}
				return
			}

			// Otherwise, wait for the next notification
			s.notificationCond.Wait()
		}
	}()

	// Wait for either the condition to be met or for a timeout
	select {
	case <-timeoutCtx.Done():
		return NewErrWaitingForStateTimeout(
			state, s.lastNotification.NextState,
		)
	case <-ch:
		return nil
	}
}

// FixedSizeSlice is a slice with a fixed size.
type FixedSizeSlice[T any] struct {
	data   []T
	maxLen int

	sync.Mutex
}

// NewFixedSlice initializes a new FixedSlice with a given maximum length.
func NewFixedSizeSlice[T any](maxLen int) *FixedSizeSlice[T] {
	return &FixedSizeSlice[T]{
		data:   make([]T, 0, maxLen),
		maxLen: maxLen,
	}
}

// Add appends a new element to the slice. If the slice reaches its maximum
// length, the first element is removed.
func (fs *FixedSizeSlice[T]) Add(element T) {
	fs.Lock()
	defer fs.Unlock()

	if len(fs.data) == fs.maxLen {
		// Remove the first element
		fs.data = fs.data[1:]
	}
	// Add the new element
	fs.data = append(fs.data, element)
}

// Get returns a copy of the slice.
func (fs *FixedSizeSlice[T]) Get() []T {
	fs.Lock()
	defer fs.Unlock()

	data := make([]T, len(fs.data))
	copy(data, fs.data)

	return data
}

// GetElement returns the element at the given index.
func (fs *FixedSizeSlice[T]) GetElement(index int) T {
	fs.Lock()
	defer fs.Unlock()

	return fs.data[index]
}
