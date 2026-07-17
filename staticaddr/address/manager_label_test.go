package address

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/txscript"
	"github.com/stretchr/testify/require"
)

// TestNewAddressLabels verifies that labels belong to individual receive
// addresses, not the shared root or automatically generated change.
func TestNewAddressLabels(t *testing.T) {
	fixture := NewAddressManagerTestContext(t)
	manager := fixture.manager
	ctx := t.Context()
	var received []*AddressParameters

	for _, label := range []string{"treasury", "treasury", ""} {
		addr, _, err := manager.NewAddress(ctx, label)
		require.NoError(t, err)
		pkScript, err := txscript.PayToAddrScript(addr)
		require.NoError(t, err)
		params := manager.GetParameters(pkScript)
		require.NotNil(t, params)
		require.Equal(t, label, params.Label)
		for _, previous := range received {
			require.NotEqual(t, previous.ID, params.ID)
			require.NotEqual(t, previous.PkScript, params.PkScript)
		}
		received = append(received, params)
	}

	root, err := manager.EnsureStaticAddressRoot(ctx)
	require.NoError(t, err)
	require.Empty(t, root.Label)
	require.NoError(t, manager.UpdateStaticAddressLabel(
		ctx, root.PkScript, "legacy",
	))
	updatedRoot, err := manager.EnsureStaticAddressRoot(ctx)
	require.NoError(t, err)
	require.Equal(t, "legacy", updatedRoot.Label)
	require.Empty(t, root.Label)

	change, err := manager.NewChangeAddress(ctx)
	require.NoError(t, err)
	require.Empty(t, change.Label)

	for _, label := range []string{"operations", ""} {
		require.NoError(t, manager.UpdateStaticAddressLabel(
			ctx, received[0].PkScript, label,
		))
		require.Equal(t, label,
			manager.GetParameters(received[0].PkScript).Label)
		require.Equal(t, "treasury",
			manager.GetParameters(received[1].PkScript).Label)
		require.Equal(t, "treasury", received[0].Label)
	}

	// Rebuilding the index from SQL must restore each label independently.
	restarted, err := NewManager(manager.cfg, manager.currentHeight.Load())
	require.NoError(t, err)
	require.NoError(t, restarted.loadActiveAddresses(ctx))
	for _, params := range append(received, updatedRoot, change) {
		require.Equal(t, manager.GetParameters(params.PkScript).Label,
			restarted.GetParameters(params.PkScript).Label)
	}
	fixture.mockStaticAddressClient.AssertNumberOfCalls(
		t, "ServerNewAddress", 1,
	)
}

type failingLabelStore struct {
	Store

	err error
}

func (s *failingLabelStore) UpdateStaticAddressLabel(context.Context,
	[]byte, string) error {

	return s.err
}

// TestUpdateLabelStoreFailure verifies that failed writes do not publish new
// metadata to the active index.
func TestUpdateLabelStoreFailure(t *testing.T) {
	fixture := NewAddressManagerTestContext(t)
	manager := fixture.manager
	params, err := manager.NewReceiveAddress(t.Context(), "original")
	require.NoError(t, err)
	store := manager.cfg.Store
	writeErr := errors.New("label write failed")
	manager.cfg.Store = &failingLabelStore{Store: store, err: writeErr}

	err = manager.UpdateStaticAddressLabel(
		t.Context(), params.PkScript, "replacement",
	)
	require.ErrorIs(t, err, writeErr)
	require.Same(t, params, manager.GetParameters(params.PkScript))
	require.Equal(t, "original", params.Label)
	stored, err := store.GetAllStaticAddresses(t.Context())
	require.NoError(t, err)
	require.Equal(t, "original", stored[1].Label)
}

type blockingLabelStore struct {
	Store

	started chan struct{}
	release chan struct{}
}

func (s *blockingLabelStore) UpdateStaticAddressLabel(ctx context.Context,
	pkScript []byte, label string) error {

	close(s.started)
	select {
	case <-s.release:
		return s.Store.UpdateStaticAddressLabel(ctx, pkScript, label)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestLabelUpdateDoesNotBlockReads verifies that a pending SQL write does not
// block index lookups, and that publication leaves readers' snapshots intact.
func TestLabelUpdateDoesNotBlockReads(t *testing.T) {
	fixture := NewAddressManagerTestContext(t)
	manager := fixture.manager
	params, err := manager.NewReceiveAddress(t.Context(), "original")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store := &blockingLabelStore{
		Store: manager.cfg.Store, started: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager.cfg.Store = store
	done := make(chan error, 1)
	go func() {
		done <- manager.UpdateStaticAddressLabel(
			ctx, params.PkScript, "replacement",
		)
	}()
	select {
	case <-store.started:
	case <-ctx.Done():
		t.Fatal("label write did not start")
	}

	read := make(chan *AddressParameters, 1)
	go func() {
		read <- manager.GetParameters(params.PkScript)
	}()
	select {
	case snapshot := <-read:
		require.Same(t, params, snapshot)
		require.Equal(t, "original", snapshot.Label)
	case <-ctx.Done():
		t.Fatal("index read blocked on label write")
	}

	close(store.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("label write did not finish")
	}
	require.Equal(t, "original", params.Label)
	require.Equal(t, "replacement",
		manager.GetParameters(params.PkScript).Label)
}

// TestConcurrentLabelUpdates verifies that concurrent writers leave the SQL
// record, active index and cached root on the same committed label.
func TestConcurrentLabelUpdates(t *testing.T) {
	fixture := NewAddressManagerTestContext(t)
	manager := fixture.manager
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	root, err := manager.EnsureStaticAddressRoot(ctx)
	require.NoError(t, err)
	start := make(chan struct{})
	results := make(chan error, 3)
	for _, label := range []string{"treasury", "operations", ""} {
		go func() {
			<-start
			results <- manager.UpdateStaticAddressLabel(
				ctx, root.PkScript, label,
			)
		}()
	}
	close(start)
	for range cap(results) {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("concurrent updates did not finish")
		}
	}

	stored, err := manager.GetLegacyParameters(ctx)
	require.NoError(t, err)
	active := manager.GetParameters(root.PkScript)
	cachedRoot, err := manager.EnsureStaticAddressRoot(ctx)
	require.NoError(t, err)
	require.Equal(t, stored.Label, active.Label)
	require.Same(t, active, cachedRoot)
	require.Empty(t, root.Label)
}
