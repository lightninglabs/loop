package reservation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func startClientManager(t *testing.T, cfg *Config,
	maxActiveReservations int) (*Manager, func()) {

	t.Helper()
	m, err := NewManager(ManagerConfig{
		FSM:                   cfg,
		PollInterval:          time.Hour,
		CallTimeout:           5 * time.Second,
		MaxActiveReservations: maxActiveReservations,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- m.Run(ctx) }()
	require.NoError(t, m.WaitInitComplete(t.Context()))
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-result:
			require.ErrorIs(t, err, context.Canceled)

		case <-time.After(2 * time.Second):
			t.Fatal("reservation manager did not stop")
		}
	})
	t.Cleanup(stop)
	return m, stop
}

func TestClientManagerDuplicateRequests(t *testing.T) {
	h := newClientHarness(t)
	h.store.Store = NewSqlStore(loopdb.NewTestDB(t).BaseDB)
	h.store.Store.(*SqlStore).clock = h.cfg.Clock
	m, stop := startClientManager(t, h.cfg, 1)
	ctx := t.Context()
	results := make(chan error, 4)
	var requests sync.WaitGroup
	for range 4 {
		requests.Go(func() {
			_, err := m.NewPurchase(ctx, h.id, [32]byte{2}, 10000, false)
			results <- err
		})
	}
	requests.Wait()
	for range 4 {
		require.NoError(t, <-results)
	}
	require.Eventually(t, func() bool {
		r, err := m.Wake(ctx, h.id)
		return err == nil && r.State == AwaitApproval
	}, 2*time.Second, time.Millisecond)
	_, err := m.NewPurchase(ctx, h.id, [32]byte{2}, 10001, false)
	require.ErrorIs(t, err, ErrRequestConflict)
	_, err = m.NewPurchase(ctx, ID{9}, [32]byte{2}, 10000, false)
	require.ErrorIs(t, err, ErrReservationLimit)
	reservations, err := m.List(ctx, StateFilter{})
	require.NoError(t, err)
	require.Len(t, reservations, 1)
	// An unapproved purchase expires without a caller canceling it.
	h.cfg.Clock.(*clock.TestClock).SetTime(time.Unix(h.quote.ExpiresAt, 0))
	_, err = m.Wake(ctx, h.id)
	require.NoError(t, err)
	r, err := m.Get(ctx, h.id)
	require.NoError(t, err)
	require.Equal(t, Canceled, r.State)
	stop()
	require.Equal(t, 1, h.deriveCalls)
	require.Equal(t, 1, h.quotes)
	require.Zero(t, h.sends)
}

func TestClientManagerRestoresBeyondAdmissionLimit(t *testing.T) {
	h := newClientHarness(t)
	h.store.Store = NewSqlStore(loopdb.NewTestDB(t).BaseDB)
	h.store.Store.(*SqlStore).clock = h.cfg.Clock
	for _, id := range []ID{{1}, {2}} {
		r := testReservation()
		r.ID, r.State = id, AwaitApproval
		r.Quote = testPurchaseQuote(r)
		// These purchases already passed their probes. Recovery should
		// restore their workers without new node calls or state writes.
		r.Probes = ProbeResults{
			Main: ProbeSucceeded, CheckedAt: h.cfg.Clock.Now(),
		}
		require.NoError(t, h.store.CreateReservation(t.Context(), r))
	}
	m, stop := startClientManager(t, h.cfg, 1)
	for _, id := range []ID{{1}, {2}} {
		r, err := m.Wake(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, AwaitApproval, r.State)
	}
	_, err := m.NewPurchase(t.Context(), ID{3}, [32]byte{2}, 10000, false)
	require.ErrorIs(t, err, ErrReservationLimit)
	stop()
	require.Zero(t, h.sends)
	require.Zero(t, h.quotes)
}

func TestClientManagerRecoveryFiltersCompleted(t *testing.T) {
	h := newClientHarness(t)
	db := loopdb.NewTestDB(t)
	h.store.Store = NewSqlStore(db.BaseDB)
	active := testReservation()
	active.Quote = testPurchaseQuote(active)
	require.NoError(t, h.store.CreateReservation(t.Context(), active))
	for i, state := range []fsm.StateType{
		QuoteFailed, QuoteRejected, Canceled, Expired,
	} {
		r := testReservation()
		r.ID, r.State = ID{byte(i + 2)}, state
		require.NoError(t, h.store.CreateReservation(t.Context(), r))
		// Loading these snapshots would fail validation. Recovery must
		// exclude them in the database, before decoding their quotes.
		_, err := db.ExecContext(t.Context(), `UPDATE asset_reservations
			SET quote = $1 WHERE reservation_id = $2`, []byte{0xff}, r.ID[:])
		require.NoError(t, err)
	}
	m, stop := startClientManager(t, h.cfg, 1)
	r, err := m.Wake(t.Context(), active.ID)
	require.NoError(t, err)
	require.Equal(t, AwaitApproval, r.State)
	rows, err := m.List(t.Context(), StateFilter{
		State: AwaitApproval,
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	rows, err = m.List(t.Context(), StateFilter{ActiveOnly: true})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	stop()
}

func TestClientManagerCancelsPendingNodeCalls(t *testing.T) {
	h := newClientHarness(t)
	h.blockQuote = true
	h.quoteStarted = make(chan struct{}, 1)
	_, stop := startClientManager(t, h.cfg, 1)
	select {
	case <-h.quoteStarted:

	case <-time.After(2 * time.Second):
		t.Fatal("quote call did not start")
	}
	stop()
	require.Zero(t, h.sends)
	r := h.record()
	require.Equal(t, RequestQuote, r.State)
}

func TestClientManagerSkipsProbeAcrossRestart(t *testing.T) {
	h := newClientHarness(t)
	h.store.Store = NewSqlStore(loopdb.NewTestDB(t).BaseDB)
	h.store.Store.(*SqlStore).clock = h.cfg.Clock
	h.blockQuote = true
	h.quoteStarted = make(chan struct{}, 1)
	m, stop := startClientManager(t, h.cfg, 1)
	_, err := m.NewPurchase(t.Context(), h.id, [32]byte{2}, 10000, true)
	require.NoError(t, err)
	select {
	case <-h.quoteStarted:

	case <-time.After(2 * time.Second):
		t.Fatal("quote call did not start")
	}
	stop()
	require.True(t, h.record().SkipProbe)
	require.Empty(t, h.probeInvoices)

	// A restart before the quote arrives must retain the skip preference.
	h.blockQuote = false
	m, stop = startClientManager(t, h.cfg, 1)
	r, err := m.Wake(t.Context(), h.id)
	require.NoError(t, err)
	require.Equal(t, AwaitApproval, r.State)
	require.True(t, r.SkipProbe)
	require.Equal(t, ProbeResults{}, r.Probes)
	require.Equal(t, AwaitApproval, r.State)
	require.Zero(t, r.MaxRouteFeeMsat)
	require.True(t, r.SkipProbe)

	a := testApproval(t, r)
	_, err = m.Approve(t.Context(), h.id, a)
	require.Error(t, err)
	r, err = m.Get(t.Context(), h.id)
	require.NoError(t, err)
	require.Nil(t, r.PaymentRequest)
	require.Equal(t, AwaitApproval, r.State)
	require.Zero(t, r.MaxRouteFeeMsat)
	require.True(t, r.SkipProbe)

	// Skipping alone cannot pay. The approval must explicitly accept the
	// missing probe, and storage must retain that consent for recovery.
	a.SkipProbe = true
	r, err = m.Approve(t.Context(), h.id, a)
	require.ErrorContains(t, err, "lost send reply")
	require.Equal(t, a.MaxRouteFeeMsat, r.MaxRouteFeeMsat)
	require.Equal(t, a.SkipProbe, r.SkipProbe)
	stop()
	require.Empty(t, h.probeInvoices)
	require.Equal(t, 1, h.sends)
	h.restart()
	h.event(OnRecover, nil)
	require.Equal(t, 1, h.sends)
	require.Empty(t, h.probeInvoices)
}

// blockedCreationWallet holds key derivation until its context is canceled.
type blockedCreationWallet struct {
	ReservationVerifier

	started chan struct{}
}

func (w *blockedCreationWallet) DeriveKey(ctx context.Context) (
	*keychain.KeyDescriptor, error) {

	close(w.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

// blockedSnapshotStore lets creation load an existing reservation, then holds
// the response snapshot read until its context is canceled.
type blockedSnapshotStore struct {
	Store

	started chan struct{}
	reads   int
}

func (s *blockedSnapshotStore) GetReservation(ctx context.Context,
	id ID) (*Reservation, error) {

	s.reads++
	if s.reads == 2 {
		close(s.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.Store.GetReservation(ctx, id)
}

// TestClientManagerCancelsCreation checks shutdown while a new-purchase request
// is waiting on key derivation or the saved snapshot returned to its caller.
func TestClientManagerCancelsCreation(t *testing.T) {
	for _, phase := range []string{"key derivation", "snapshot read"} {
		t.Run(phase, func(t *testing.T) {
			h := newClientHarness(t)
			started := make(chan struct{})
			if phase == "key derivation" {
				h.store.Store = NewSqlStore(loopdb.NewTestDB(t).BaseDB)
				h.store.Store.(*SqlStore).clock = h.cfg.Clock
				h.cfg.Wallet = &blockedCreationWallet{
					ReservationVerifier: h.cfg.Wallet,
					started:             started,
				}
			} else {
				// A final reservation has no worker, so only the
				// creation request reads it during this test.
				r := h.record()
				r.State = Canceled
				require.NoError(t, h.store.UpdateReservation(t.Context(), r))
				h.cfg.Store = &blockedSnapshotStore{
					Store:   h.store,
					started: started,
				}
			}

			m, err := NewManager(ManagerConfig{
				FSM:                   h.cfg,
				PollInterval:          time.Hour,
				CallTimeout:           5 * time.Second,
				MaxActiveReservations: 1,
			})
			require.NoError(t, err)
			runCtx, cancelRun := context.WithCancel(t.Context())
			requestCtx, cancelRequest := context.WithCancel(t.Context())
			done := make(chan struct{})
			var runErr error
			go func() {
				runErr = m.Run(runCtx)
				close(done)
			}()
			t.Cleanup(func() {
				cancelRequest()
				cancelRun()
				select {
				case <-done:

				case <-time.After(2 * time.Second):
					t.Error("reservation manager did not stop after cleanup")
				}
			})
			require.NoError(t, m.WaitInitComplete(t.Context()))
			response := make(chan error, 1)
			go func() {
				_, err := m.NewPurchase(
					requestCtx, h.id, [32]byte{2}, 10000, false,
				)
				response <- err
			}()
			select {
			case <-started:

			case <-time.After(2 * time.Second):
				t.Fatal("creation call did not start")
			}

			cancelRun()
			select {
			case <-done:
				require.ErrorIs(t, runErr, context.Canceled)

			case <-time.After(time.Second):
				t.Fatal("shutdown did not cancel the creation call")
			}
			require.NoError(t, requestCtx.Err())
			select {
			case err := <-response:
				require.Error(t, err)

			case <-time.After(time.Second):
				t.Fatal("purchase caller did not return")
			}
		})
	}
}
