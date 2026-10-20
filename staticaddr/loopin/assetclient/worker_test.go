package assetclient

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type workerActions struct {
	sessionActions
	creates atomic.Int32
	onPush  func()
}

func (a *workerActions) CreateInvoice(context.Context, assetexec.Offer,
	[]byte) (assetexec.Invoice, error) {

	a.creates.Add(1)
	return assetexec.Invoice{
		PaymentRequest: "original-invoice",
		PaymentPlan:    []byte("fixed-rfq"),
	}, nil
}

func (a *workerActions) PushSignatures(context.Context, assetexec.Offer,
	[]byte) error {

	if a.onPush != nil {
		a.onPush()
	}
	return nil
}

type workerServer struct {
	testServer
	prepare func(context.Context,
		*swapserverrpc.PrepareAssetLoopInRequest) error
}

func (s *workerServer) PrepareAssetLoopIn(ctx context.Context,
	r *swapserverrpc.PrepareAssetLoopInRequest, _ ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	return &swapserverrpc.AssetLoopInStatus{}, s.prepare(ctx, r)
}

func TestSessionWorkerResumesReopenedJournalAndRetries(t *testing.T) {
	_, _, offer := offerFixture(t)
	path := filepath.Join(t.TempDir(), "client.db")
	store, err := assetexec.OpenBoltStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	actions := &workerActions{}
	controller := assetexec.NewController(store, actions, nil)
	intent, err := json.Marshal(InvoiceIntent{
		RecoveryRequest: []byte("original-request"),
	})
	require.NoError(t, err)
	require.NoError(t, controller.Start(t.Context(), offer, intent))
	_, err = controller.PrepareInvoice(t.Context(), offer.Hash)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = assetexec.OpenBoltStore(path)
	require.NoError(t, err)
	controller = assetexec.NewController(store, actions, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	actions.onPush = cancel
	var calls atomic.Int32
	server := &workerServer{prepare: func(_ context.Context,
		r *swapserverrpc.PrepareAssetLoopInRequest) error {

		require.Equal(t, offer.Hash[:], r.PaymentHash)
		require.Equal(t, "original-invoice", r.AssetHoldInvoice)
		require.Equal(t, []byte("original-request"), r.RecoveryRequest)
		if calls.Add(1) == 1 {
			return errLost
		}
		return nil
	}}
	server.fallback = func() ([]byte, error) {
		return []byte("original-fallback"), nil
	}
	session := &Session{
		Controller: controller,
		Adapter: &Adapter{
			Store: store, Actions: actions, Server: server,
		},
	}
	initialized := make(chan struct{})
	failures := make(chan error, 2)
	result := make(chan error, 1)
	go func() {
		result <- session.Run(ctx, WorkerConfig{
			Initialized: initialized, Interval: time.Millisecond,
			ActionTimeout: time.Second, Concurrency: 1,
			OnError: func(_ [32]byte, err error) {
				failures <- err
			},
		})
	}()
	// A daemon must finish signer and deposit-manager initialization before
	// a recovered record can send even its first peer request.
	require.Never(t, func() bool { return calls.Load() > 0 },
		20*time.Millisecond, time.Millisecond)
	close(initialized)
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish after cancellation")
	}
	require.ErrorIs(t, <-failures, errLost)
	require.EqualValues(t, 2, calls.Load())
	require.EqualValues(t, 1, actions.creates.Load())
	record, err := store.Get(t.Context(), offer.Hash)
	require.NoError(t, err)
	require.True(t, record.SignaturesExposed)
	require.Equal(t, []byte("signed-sdk-bundle"), record.Fallback.Bundle)
}

func TestSessionWorkerBoundsConcurrentActions(t *testing.T) {
	_, _, offer := offerFixture(t)
	store, err := assetexec.OpenBoltStore(
		filepath.Join(t.TempDir(), "client.db"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	actions := &workerActions{}
	controller := assetexec.NewController(store, actions, nil)
	intent, err := json.Marshal(InvoiceIntent{
		RecoveryRequest: []byte("fixed-request"),
	})
	require.NoError(t, err)
	for i := range 4 {
		candidate := offer
		candidate.Hash[0] = byte(i + 1)
		candidate.Terms.DepositOutpoints = []string{
			offer.Terms.DepositOutpoints[0][:64] + ":" +
				strconv.Itoa(i),
		}
		require.NoError(
			t,
			controller.Start(t.Context(), candidate, intent),
		)
	}
	var active, maximum atomic.Int32
	server := &workerServer{prepare: func(ctx context.Context,
		_ *swapserverrpc.PrepareAssetLoopInRequest) error {

		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous ||
				maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	session := &Session{
		Controller: controller,
		Adapter: &Adapter{
			Store: store, Actions: actions, Server: server,
		},
	}
	initialized := make(chan struct{})
	close(initialized)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failures := make(chan error, 4)
	err = session.Run(ctx, WorkerConfig{
		Initialized: initialized, Interval: time.Hour,
		ActionTimeout: 100 * time.Millisecond, Concurrency: 2,
		OnReady: cancel,
		OnError: func(_ [32]byte, err error) {
			failures <- err
		},
	})
	require.ErrorIs(t, err, context.Canceled)
	require.EqualValues(t, 2, maximum.Load())
	require.Zero(t, active.Load())
	require.Len(t, failures, 4)
	for range 4 {
		require.ErrorIs(t, <-failures, context.DeadlineExceeded)
	}
}

type failingScanStore struct {
	assetexec.Store
	calls atomic.Int32
}

func (s *failingScanStore) ListActive(context.Context) (
	[]*assetexec.Record, error) {

	s.calls.Add(1)
	return nil, errLost
}

func TestSessionWorkerLifecycle(t *testing.T) {
	store := &failingScanStore{}
	session := &Session{
		Controller: assetexec.NewController(store, nil, nil),
		Adapter:    &Adapter{Store: store},
	}
	initialized := make(chan struct{})
	cfg := WorkerConfig{
		Initialized: initialized, Interval: time.Hour,
		ActionTimeout: time.Second, Concurrency: 1,
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, session.Run(ctx, cfg), context.Canceled)
	require.Zero(t, store.calls.Load())
	close(initialized)
	// Storage failures stop recovery rather than pretending that no active
	// swaps exist. The same session can be restarted once its store works.
	require.ErrorIs(t, session.Run(t.Context(), cfg), errLost)
	require.EqualValues(t, 1, store.calls.Load())
	session.running.Store(true)
	require.ErrorIs(t, session.Run(t.Context(), cfg), assetexec.ErrState)
	session.running.Store(false)
	cfg.Concurrency = 0
	require.ErrorIs(t, session.Run(t.Context(), cfg), assetexec.ErrInvalid)
}
