package loopin

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetclient"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/stretchr/testify/require"
)

type assetLifecycleStore struct {
	AssetStore

	scanError error
	scans     chan struct{}
	inventory func(context.Context) error
}

func (s *assetLifecycleStore) ListActive(ctx context.Context) (
	[]*assetexec.Record, error) {

	if s.scans != nil {
		select {
		case s.scans <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, s.scanError
}

func (s *assetLifecycleStore) AssetInventory(ctx context.Context) (
	[]AssetInventoryEntry, error) {

	if s.inventory != nil {
		return nil, s.inventory(ctx)
	}
	return nil, nil
}

func (*assetLifecycleStore) AssetJobs(context.Context) ([]AssetJob, error) {
	return nil, nil
}

type assetLifecycleDriver struct{ AssetDriver }

func (assetLifecycleDriver) Height(context.Context) (uint32, error) {
	return 100, nil
}

type assetLifecycleNotifier struct {
	lndclient.ChainNotifierClient

	blocks chan int32
}

func (n assetLifecycleNotifier) RegisterBlockEpochNtfn(context.Context) (
	chan int32, chan error, error) {

	return n.blocks, make(chan error), nil
}

type assetLifecycleBitcoinStore struct {
	mockStore

	recover func(context.Context) error
}

func (s *assetLifecycleBitcoinStore) GetStaticAddressLoopInSwapsByStates(
	ctx context.Context, _ []fsm.StateType) ([]*StaticAddressLoopIn, error) {

	return nil, s.recover(ctx)
}

func assetLifecycleManager(t *testing.T, store AssetStore) (
	*Manager, chan int32) {

	t.Helper()
	blocks := make(chan int32)
	m, err := NewManager(&Config{
		ChainNotifier:       assetLifecycleNotifier{blocks: blocks},
		NotificationManager: &mockNotificationManager{},
		Store:               &mockStore{},
	}, 1)
	require.NoError(t, err)
	m.assetCfg = &AssetConfig{Store: store, Driver: assetLifecycleDriver{}}
	m.assetSession = &assetclient.Session{
		Controller: assetexec.NewController(store, nil, nil),
		Adapter:    &assetclient.Adapter{Store: store},
	}
	return m, blocks
}

// TestAssetRecoveryManagerLifecycle verifies the shared manager starts asset
// recovery after Bitcoin recovery, keeps accepting block notifications during
// a blocked runtime call, and joins that call before its own shutdown returns.
func TestAssetRecoveryManagerLifecycle(t *testing.T) {
	bitcoinStarted := make(chan struct{})
	bitcoinReady := make(chan struct{})
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var calls atomic.Int32
	inventory := func(ctx context.Context) error {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	}
	scans := make(chan struct{}, 3)
	m, blocks := assetLifecycleManager(t, &assetLifecycleStore{
		scans: scans, inventory: inventory,
	})
	m.cfg.Store = &assetLifecycleBitcoinStore{
		recover: func(ctx context.Context) error {
			close(bitcoinStarted)
			select {
			case <-bitcoinReady:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	initialized := make(chan struct{})
	finished := make(chan error, 1)
	go func() { finished <- m.Run(ctx, initialized) }()
	receiveOrFail(t, bitcoinStarted)
	require.Zero(t, calls.Load())
	select {
	case <-initialized:
		t.Fatal("manager initialized before Bitcoin recovery")
	default:
	}
	close(bitcoinReady)
	receiveOrFail(t, initialized)
	receiveOrFail(t, started)
	receiveOrFail(t, scans)
	// Pending admission/runtime work must not delay the next batch of
	// already exposed swaps, whose independent recovery has a deadline.
	select {
	case <-scans:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked request suppressed active swap recovery")
	}
	blocks <- 123
	require.Eventually(t, func() bool {
		return m.currentHeight.Load() == 123
	}, time.Second, time.Millisecond)
	cancel()
	receiveOrFail(t, canceled)
	select {
	case err := <-finished:
		t.Fatalf("manager returned before runtime joined: %v", err)
	default:
	}
	close(release)
	require.ErrorIs(t, receiveOrFail(t, finished), context.Canceled)
	require.EqualValues(t, 1, calls.Load())
}

func TestAssetRecoveryManagerReschedulesAndStopsOnStoreFailure(t *testing.T) {
	calls := make(chan struct{}, 2)
	m, _ := assetLifecycleManager(t, &assetLifecycleStore{
		inventory: func(context.Context) error { calls <- struct{}{}; return nil },
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	initialized := make(chan struct{})
	finished := make(chan error, 1)
	go func() { finished <- m.Run(ctx, initialized) }()
	receiveOrFail(t, initialized)
	receiveOrFail(t, calls)
	select {
	case <-calls:
	case <-time.After(3 * time.Second):
		t.Fatal("manager did not schedule another recovery batch")
	}
	cancel()
	require.ErrorIs(t, receiveOrFail(t, finished), context.Canceled)

	lost := errors.New("asset database unavailable")
	m, _ = assetLifecycleManager(t, &assetLifecycleStore{scanError: lost})
	require.ErrorIs(t, m.Run(t.Context(), make(chan struct{})), lost)
}
