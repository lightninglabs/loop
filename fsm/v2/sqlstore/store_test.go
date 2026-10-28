package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"

	fsm "github.com/lightninglabs/loop/fsm/v2"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // Register the real SQLite test driver.
)

func openDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})
	require.NoError(t, db.PingContext(t.Context()))
	_, err = db.ExecContext(t.Context(), "PRAGMA synchronous = FULL")
	require.NoError(t, err)

	return db
}

func testStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()

	db := openDatabase(t, filepath.Join(t.TempDir(), "fsm.sqlite"))
	_, err := db.ExecContext(t.Context(), Schema)
	require.NoError(t, err)
	store, err := New(db)
	require.NoError(t, err)

	return store, db
}

func checkpoint() fsm.Record {
	return fsm.Record{
		ID:       "swap-one",
		Version:  1,
		Revision: 1,
		State:    "prepared",
		Data:     []byte("durable-effect-intent"),
	}
}

// TestRestart checks committed data survives closing all database handles and
// constructing an entirely new store, without any retained machine memory.
func TestRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "fsm.sqlite")
	db := openDatabase(t, path)
	_, err := db.ExecContext(ctx, Schema)
	require.NoError(t, err)
	store, err := New(db)
	require.NoError(t, err)
	record := checkpoint()
	require.NoError(t, store.Create(ctx, record))
	require.ErrorIs(t, store.Create(ctx, record), fsm.ErrConflict)

	record.Revision++
	record.State = "published"
	record.Data = []byte("signed-transaction-and-operation-id")
	require.NoError(t, store.CompareAndSwap(ctx, record, 1))
	require.NoError(t, db.Close())

	reopened, err := New(openDatabase(t, path))
	require.NoError(t, err)
	loaded, err := reopened.Load(ctx, record.ID)
	require.NoError(t, err)
	require.Equal(t, record, loaded)
}

func TestOwnershipAndCAS(t *testing.T) {
	ctx := t.Context()
	store, _ := testStore(t)
	initial := checkpoint()
	require.NoError(t, store.Create(ctx, initial))
	initial.Data[0] = 'X'

	loaded, err := store.Load(ctx, initial.ID)
	require.NoError(t, err)
	require.Equal(t, checkpoint(), loaded)
	loaded.Data[0] = 'Y'
	reloaded, err := store.Load(ctx, initial.ID)
	require.NoError(t, err)
	require.Equal(t, checkpoint(), reloaded)

	next := checkpoint()
	next.Revision = 2
	next.State = "monitoring"
	next.Data = []byte("monitor-operation")
	require.NoError(t, store.CompareAndSwap(ctx, next, 1))
	require.ErrorIs(t, store.CompareAndSwap(ctx, next, 1), fsm.ErrConflict)

	// Changing the definition version requires explicit migration, not a
	// regular application transition.
	incompatible := next
	incompatible.Version++
	incompatible.Revision++
	require.ErrorIs(t, store.CompareAndSwap(ctx, incompatible, 2),
		fsm.ErrConflict)

	loaded, err = store.Load(ctx, initial.ID)
	require.NoError(t, err)
	require.Equal(t, next, loaded)

	next.ID = "missing"
	require.ErrorIs(t, store.CompareAndSwap(ctx, next, 1), fsm.ErrConflict)
	_, err = store.Load(ctx, next.ID)
	require.ErrorIs(t, err, fsm.ErrNotFound)
}

func TestConcurrentWriters(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "fsm.sqlite")
	db := openDatabase(t, path)
	_, err := db.ExecContext(ctx, Schema)
	require.NoError(t, err)
	first, err := New(db)
	require.NoError(t, err)
	second, err := New(openDatabase(t, path))
	require.NoError(t, err)
	require.NoError(t, first.Create(ctx, checkpoint()))

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, store := range []*Store{first, second} {
		wg.Go(func() {
			<-start
			next := checkpoint()
			next.Revision = 2
			next.Data = []byte{byte(i + 1)}
			results <- store.CompareAndSwap(ctx, next, 1)
		})
	}
	close(start)
	wg.Wait()
	close(results)

	var successes, conflicts int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, fsm.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected CAS error: %v", err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
	loaded, err := first.Load(ctx, checkpoint().ID)
	require.NoError(t, err)
	require.Equal(t, uint64(2), loaded.Revision)
	require.Contains(t, []byte{1, 2}, loaded.Data[0])
}

func TestInvalidWrites(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	require.NoError(t, store.Create(ctx, checkpoint()))

	for name, mutate := range map[string]func(*fsm.Record){
		"empty id":      func(r *fsm.Record) { r.ID = "" },
		"zero version":  func(r *fsm.Record) { r.Version = 0 },
		"zero revision": func(r *fsm.Record) { r.Revision = 0 },
		"overflow revision": func(r *fsm.Record) {
			r.Revision = fsm.MaxRevision + 1
		},
		"empty state": func(r *fsm.Record) { r.State = "" },
		"empty data":  func(r *fsm.Record) { r.Data = nil },
	} {
		t.Run(name, func(t *testing.T) {
			record := checkpoint()
			mutate(&record)
			require.ErrorIs(t, store.Create(ctx, record), fsm.ErrInvalidRecord)
			require.ErrorIs(t, store.CompareAndSwap(ctx, record, 1),
				fsm.ErrInvalidRecord)
		})
	}

	for _, revision := range []uint64{2, fsm.MaxRevision} {
		record := checkpoint()
		record.Revision = revision
		require.ErrorIs(t, store.Create(ctx, record), fsm.ErrInvalidRecord)
	}
	for _, revision := range []uint64{0, 2, fsm.MaxRevision, math.MaxUint64} {
		next := checkpoint()
		next.Revision = 2
		require.ErrorIs(t, store.CompareAndSwap(ctx, next, revision),
			fsm.ErrInvalidRecord)
	}

	loaded, err := store.Load(ctx, checkpoint().ID)
	require.NoError(t, err)
	require.Equal(t, checkpoint(), loaded)
}

func TestCorruptRows(t *testing.T) {
	for name, query := range map[string]string{
		"zero version":      "UPDATE loop_fsm_v2 SET version = 0",
		"overflow version":  "UPDATE loop_fsm_v2 SET version = 4294967296",
		"negative revision": "UPDATE loop_fsm_v2 SET revision = -1",
		"zero revision":     "UPDATE loop_fsm_v2 SET revision = 0",
		"empty state":       "UPDATE loop_fsm_v2 SET state = ''",
		"empty data":        "UPDATE loop_fsm_v2 SET data = X''",
	} {
		t.Run(name, func(t *testing.T) {
			store, db := testStore(t)
			ctx := t.Context()
			require.NoError(t, store.Create(ctx, checkpoint()))
			_, err := db.ExecContext(ctx, "PRAGMA ignore_check_constraints = ON")
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, query)
			require.NoError(t, err)
			_, err = store.Load(ctx, checkpoint().ID)
			require.ErrorIs(t, err, fsm.ErrInvalidRecord)
		})
	}
}

func TestCanceledOperations(t *testing.T) {
	store, _ := testStore(t)
	require.NoError(t, store.Create(t.Context(), checkpoint()))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	record := checkpoint()
	require.ErrorIs(t, store.Create(ctx, record), context.Canceled)
	_, err := store.Load(ctx, record.ID)
	require.ErrorIs(t, err, context.Canceled)
	record.Revision++
	require.ErrorIs(t, store.CompareAndSwap(ctx, record, 1), context.Canceled)

	loaded, err := store.Load(t.Context(), record.ID)
	require.NoError(t, err)
	require.Equal(t, checkpoint(), loaded)
}

func TestNilDatabase(t *testing.T) {
	_, err := New(nil)
	require.ErrorIs(t, err, fsm.ErrConfig)
}
