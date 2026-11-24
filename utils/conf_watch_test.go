package utils

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

const (
	confWatchTestTimeout = 5 * time.Second
	confWatchTestDelay   = 10 * time.Second
)

// errTestNotifier is the notifier failure used by the tests.
var errTestNotifier = errors.New("notifier stream closed")

// confWatchReg is one registration handed out by confWatchNotifier.
type confWatchReg struct {
	ctx      context.Context
	txid     *chainhash.Hash
	pkScript []byte
	numConfs int32
	hint     int32
	numOpts  int
	confChan chan *chainntnfs.TxConfirmation
	errChan  chan error
}

// confWatchNotifier is a chain notifier that fails registrations on request
// and, like lnd, creates new channels for every registration.
type confWatchNotifier struct {
	mu sync.Mutex

	// regErrs are returned by the next registrations, in order.
	regErrs []error

	// regs receives every successful registration.
	regs chan *confWatchReg
}

// newConfWatchNotifier creates a notifier whose registrations succeed.
func newConfWatchNotifier() *confWatchNotifier {
	return &confWatchNotifier{
		regs: make(chan *confWatchReg, 10),
	}
}

// RegisterConfirmationsNtfn fails the registration or hands out a new one.
func (n *confWatchNotifier) RegisterConfirmationsNtfn(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs, heightHint int32,
	opts ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

	n.mu.Lock()
	defer n.mu.Unlock()

	if len(n.regErrs) > 0 {
		err := n.regErrs[0]
		n.regErrs = n.regErrs[1:]

		return nil, nil, err
	}

	reg := &confWatchReg{
		ctx:      ctx,
		txid:     txid,
		pkScript: pkScript,
		numConfs: numConfs,
		hint:     heightHint,
		numOpts:  len(opts),
		confChan: make(chan *chainntnfs.TxConfirmation, 1),
		errChan:  make(chan error, 1),
	}
	n.regs <- reg

	return reg.confChan, reg.errChan, nil
}

// confWatchHarness runs WatchConfirmation against a failing notifier and a
// test clock.
type confWatchHarness struct {
	t        *testing.T
	notifier *confWatchNotifier
	clock    *clock.TestClock
	ticks    chan time.Duration
	retries  chan error
	attempts chan int
	watch    ConfWatch
}

// newConfWatchHarness creates the harness and the watch request.
func newConfWatchHarness(t *testing.T) *confWatchHarness {
	t.Helper()

	ticks := make(chan time.Duration, 1)
	testClock := clock.NewTestClockWithTickSignal(time.Unix(1, 0), ticks)
	h := &confWatchHarness{
		t:        t,
		notifier: newConfWatchNotifier(),
		clock:    testClock,
		ticks:    ticks,
		retries:  make(chan error, 10),
		attempts: make(chan int, 10),
	}

	h.watch = ConfWatch{
		Notifier:   h.notifier,
		Clock:      testClock,
		Txid:       &chainhash.Hash{1},
		PkScript:   []byte{2},
		NumConfs:   3,
		HeightHint: 100,
		Opts: []lndclient.NotifierOption{
			lndclient.WithIncludeBlock(),
		},
		RetryDelay: confWatchTestDelay,
		OnRetry: func(attempt int, err error) {
			h.attempts <- attempt
			h.retries <- err
		},
	}

	return h
}

// nextReg waits for the next successful registration.
func (h *confWatchHarness) nextReg() *confWatchReg {
	h.t.Helper()

	select {
	case reg := <-h.notifier.regs:
		return reg

	case <-time.After(confWatchTestTimeout):
		h.t.Fatal("no registration")

		return nil
	}
}

// requireRetry asserts that the watch reported the error and started its
// retry delay, then lets the delay pass.
func (h *confWatchHarness) requireRetry(attempt int, expected error) {
	h.t.Helper()

	select {
	case err := <-h.retries:
		require.ErrorIs(h.t, err, expected)
		require.Equal(h.t, attempt, <-h.attempts)

	case <-time.After(confWatchTestTimeout):
		h.t.Fatal("no retry reported")
	}

	select {
	case delay := <-h.ticks:
		require.Equal(h.t, confWatchTestDelay, delay)

	case <-time.After(confWatchTestTimeout):
		h.t.Fatal("no retry delay started")
	}

	h.clock.SetTime(h.clock.Now().Add(confWatchTestDelay))
}

// requireConf asserts that the watch delivers the confirmation.
func (h *confWatchHarness) requireConf(
	confChan <-chan *chainntnfs.TxConfirmation,
	expected *chainntnfs.TxConfirmation) {

	h.t.Helper()

	select {
	case conf := <-confChan:
		require.Same(h.t, expected, conf)

	case <-time.After(confWatchTestTimeout):
		h.t.Fatal("no confirmation delivered")
	}
}

// TestWatchConfirmation verifies that the watch passes its parameters to the
// notifier and delivers the confirmation.
func TestWatchConfirmation(t *testing.T) {
	t.Parallel()

	h := newConfWatchHarness(t)
	confChan := WatchConfirmation(t.Context(), h.watch)

	reg := h.nextReg()
	require.Equal(t, h.watch.Txid, reg.txid)
	require.Equal(t, h.watch.PkScript, reg.pkScript)
	require.Equal(t, h.watch.NumConfs, reg.numConfs)
	require.Equal(t, h.watch.HeightHint, reg.hint)
	require.Equal(t, 1, reg.numOpts)

	conf := &chainntnfs.TxConfirmation{BlockHeight: 105}
	reg.confChan <- conf
	h.requireConf(confChan, conf)

	// The registration is released after the confirmation.
	select {
	case <-reg.ctx.Done():
	case <-time.After(confWatchTestTimeout):
		t.Fatal("registration not released")
	}
	require.Empty(t, h.retries)
}

// TestWatchConfirmationRetries verifies that the watch registers again after
// a delay when the registration fails, the notifier reports an error or the
// notifier closes a channel, and that it still delivers the confirmation.
func TestWatchConfirmationRetries(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string

		// regErr fails the first registration.
		regErr bool

		// fail breaks the first successful registration.
		fail func(reg *confWatchReg)

		expectedErr error
	}{
		{
			name:        "registration error",
			regErr:      true,
			expectedErr: errTestNotifier,
		},
		{
			name: "stream error",
			fail: func(reg *confWatchReg) {
				reg.errChan <- errTestNotifier
			},
			expectedErr: errTestNotifier,
		},
		{
			name: "error channel closed",
			fail: func(reg *confWatchReg) {
				close(reg.errChan)
			},
			expectedErr: errConfErrChanClosed,
		},
		{
			name: "confirmation channel closed",
			fail: func(reg *confWatchReg) {
				close(reg.confChan)
			},
			expectedErr: errConfChanClosed,
		},
		{
			name: "nil confirmation",
			fail: func(reg *confWatchReg) {
				reg.confChan <- nil
			},
			expectedErr: errNilConf,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newConfWatchHarness(t)
			if tc.regErr {
				h.notifier.regErrs = []error{errTestNotifier}
			}

			confChan := WatchConfirmation(t.Context(), h.watch)

			var failed *confWatchReg
			if tc.fail != nil {
				failed = h.nextReg()
				tc.fail(failed)
			}
			h.requireRetry(1, tc.expectedErr)

			// The failed attempt leaves no registration behind.
			if failed != nil {
				require.Error(t, failed.ctx.Err())
			}

			// A second failure is counted, and the watch goes on.
			reg := h.nextReg()
			reg.errChan <- errTestNotifier
			h.requireRetry(2, errTestNotifier)

			conf := &chainntnfs.TxConfirmation{BlockHeight: 105}
			reg = h.nextReg()
			reg.confChan <- conf
			h.requireConf(confChan, conf)
		})
	}
}

// TestWatchConfirmationStopsOnCancel verifies that a canceled watch neither
// reports an error nor registers again.
func TestWatchConfirmationStopsOnCancel(t *testing.T) {
	t.Parallel()

	t.Run("canceled while registered", func(t *testing.T) {
		t.Parallel()

		h := newConfWatchHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		confChan := WatchConfirmation(ctx, h.watch)
		reg := h.nextReg()

		// A canceled subscription reports an error, like lnd does.
		cancel()
		reg.errChan <- context.Canceled

		select {
		case <-reg.ctx.Done():
		case <-time.After(confWatchTestTimeout):
			t.Fatal("registration not released")
		}

		select {
		case <-confChan:
			t.Fatal("unexpected confirmation")

		case err := <-h.retries:
			t.Fatalf("canceled watch reported a retry: %v", err)

		case <-h.notifier.regs:
			t.Fatal("canceled watch registered again")

		case <-time.After(100 * time.Millisecond):
		}
	})

	t.Run("canceled during the delay", func(t *testing.T) {
		t.Parallel()

		h := newConfWatchHarness(t)
		h.notifier.regErrs = []error{errTestNotifier}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		confChan := WatchConfirmation(ctx, h.watch)

		select {
		case <-h.ticks:
		case <-time.After(confWatchTestTimeout):
			t.Fatal("no retry delay started")
		}

		// The delay passes only after the cancel.
		cancel()
		h.clock.SetTime(h.clock.Now().Add(confWatchTestDelay))

		select {
		case <-confChan:
			t.Fatal("unexpected confirmation")

		case <-h.notifier.regs:
			t.Fatal("canceled watch registered again")

		case <-time.After(100 * time.Millisecond):
		}
	})
}

// TestWatchConfirmationDefaultDelay verifies that a watch without a retry
// delay waits for the default delay instead of retrying in a busy loop.
func TestWatchConfirmationDefaultDelay(t *testing.T) {
	t.Parallel()

	for _, delay := range []time.Duration{0, -time.Second} {
		h := newConfWatchHarness(t)
		h.notifier.regErrs = []error{errTestNotifier}
		h.watch.RetryDelay = delay

		WatchConfirmation(t.Context(), h.watch)

		select {
		case tick := <-h.ticks:
			require.Equal(t, DefaultConfWatchRetryDelay, tick)

		case <-time.After(confWatchTestTimeout):
			t.Fatal("no retry delay started")
		}
	}
}

// TestWatchConfirmationDefaultClock verifies that the watch works without a
// clock.
func TestWatchConfirmationDefaultClock(t *testing.T) {
	t.Parallel()

	h := newConfWatchHarness(t)
	h.notifier.regErrs = []error{errTestNotifier}
	h.watch.Clock = nil
	h.watch.RetryDelay = time.Millisecond

	confChan := WatchConfirmation(t.Context(), h.watch)

	conf := &chainntnfs.TxConfirmation{BlockHeight: 105}
	reg := h.nextReg()
	reg.confChan <- conf
	h.requireConf(confChan, conf)
}
