package loop

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

const resumeTestTimeout = 5 * time.Second

// resumeTestNotifier hands out a notification channel that the test feeds.
type resumeTestNotifier struct {
	ntfnChan chan *swapserverrpc.ServerUnfinishedSwapNotification
}

// SubscribeUnfinishedSwaps returns the test's notification channel.
func (n *resumeTestNotifier) SubscribeUnfinishedSwaps(context.Context,
) <-chan *swapserverrpc.ServerUnfinishedSwapNotification {

	return n.ntfnChan
}

// resumeTestStore reports every swap the resume manager fetches.
type resumeTestStore struct {
	*loopdb.StoreMock

	fetched chan lntypes.Hash
}

// FetchLoopOutSwap reports the hash and fetches the swap from the mock.
func (s *resumeTestStore) FetchLoopOutSwap(ctx context.Context,
	hash lntypes.Hash) (*loopdb.LoopOut, error) {

	s.fetched <- hash

	return s.StoreMock.FetchLoopOutSwap(ctx, hash)
}

// newResumeTestManager creates a resume manager with a test notifier, store
// and clock, and starts it. The returned channel is closed when start returns.
func newResumeTestManager(t *testing.T, ctx context.Context,
	testClock clock.Clock) (*resumeManager, *resumeTestNotifier,
	*resumeTestStore, chan struct{}) {

	notifier := &resumeTestNotifier{
		ntfnChan: make(
			chan *swapserverrpc.ServerUnfinishedSwapNotification,
		),
	}
	store := &resumeTestStore{
		StoreMock: loopdb.NewStoreMock(t),
		fetched:   make(chan lntypes.Hash, 100),
	}
	m := &resumeManager{
		ntfnManager: notifier,
		swapStore:   store,
		clock:       testClock,
		reqChan: make(
			chan *swapserverrpc.ServerUnfinishedSwapNotification, 1,
		),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.start(ctx)
	}()

	return m, notifier, store, done
}

// requireStopped cancels the manager and waits for start to return.
func requireStopped(t *testing.T, cancel context.CancelFunc,
	done chan struct{}) {

	t.Helper()

	cancel()
	select {
	case <-done:
	case <-time.After(resumeTestTimeout):
		require.FailNow(t, "resume manager did not stop")
	}
}

// TestResumeManagerManyNotifications checks that the resume manager handles
// unfinished swap notifications that arrive in quick succession. It used to
// queue each notification on its own one slot request channel, which only
// it reads, so a second notification arriving before the first was taken
// blocked it for good.
func TestResumeManagerManyNotifications(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	_, notifier, store, done := newResumeTestManager(
		t, ctx, clock.NewTestClock(time.Unix(1, 0)),
	)

	// Swaps without updates are still initiated, so the manager leaves them
	// alone after fetching them. They are added to the mock directly,
	// since CreateLoopOut reports each swap on a channel with room for one.
	const numSwaps = 20
	hashes := make(map[lntypes.Hash]struct{}, numSwaps)
	store.Lock()
	for i := range numSwaps {
		hash := lntypes.Hash{byte(i + 1)}
		hashes[hash] = struct{}{}
		store.LoopOutSwaps[hash] = &loopdb.LoopOutContract{}
	}
	store.Unlock()

	for hash := range hashes {
		select {
		case notifier.ntfnChan <- &swapserverrpc.
			ServerUnfinishedSwapNotification{
			SwapHash: hash[:],
		}:

		case <-time.After(resumeTestTimeout):
			require.FailNow(t, "resume manager stopped taking "+
				"notifications")
		}
	}

	for range numSwaps {
		select {
		case hash := <-store.fetched:
			require.Contains(t, hashes, hash)
			delete(hashes, hash)

		case <-time.After(resumeTestTimeout):
			require.FailNow(t, "not all notifications were handled")
		}
	}

	requireStopped(t, cancel, done)
}

// TestResumeManagerRetry checks that a swap whose recovery failed is tried
// again after the retry delay, and that a pending retry ends with the manager.
func TestResumeManagerRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ticks := make(chan time.Duration, 10)
	testClock := clock.NewTestClockWithTickSignal(time.Unix(1, 0), ticks)
	_, notifier, store, done := newResumeTestManager(t, ctx, testClock)

	// The swap is not in the store, so every attempt fails.
	hash := lntypes.Hash{1}
	notifier.ntfnChan <- &swapserverrpc.ServerUnfinishedSwapNotification{
		SwapHash: hash[:],
	}

	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case fetched := <-store.fetched:
			require.Equal(t, hash, fetched)

		case <-time.After(resumeTestTimeout):
			require.FailNow(t, "attempt was not made", attempt)
		}

		// The retry waits for the delay before it is handled again.
		select {
		case delay := <-ticks:
			require.Equal(t, resumeRetryDelay, delay)

		case <-time.After(resumeTestTimeout):
			require.FailNow(t, "retry was not scheduled")
		}
		require.Empty(t, store.fetched)

		if attempt < 3 {
			testClock.SetTime(testClock.Now().Add(resumeRetryDelay))
		}
	}

	// A retry still waiting for its delay ends with the manager.
	requireStopped(t, cancel, done)
	testClock.SetTime(testClock.Now().Add(resumeRetryDelay))
	select {
	case <-store.fetched:
		require.FailNow(t, "retry ran after the manager stopped")

	case <-time.After(100 * time.Millisecond):
	}
}
