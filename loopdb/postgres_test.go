//go:build test_db_postgres
// +build test_db_postgres

package loopdb

import (
	"context"
	"database/sql"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	postgres_migrate "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/stretchr/testify/require"
)

// TestPostgresConnectionPool tests that a Postgres store bounds its connection
// pool and doesn't keep a connection checked out once it is open.
func TestPostgresConnectionPool(t *testing.T) {
	fixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	t.Cleanup(func() {
		fixture.TearDown(t)
	})

	testCases := []struct {
		name           string
		maxConnections int32
		wantMaxOpen    int
	}{
		{
			// Without a configured limit, the store uses the same
			// default limit as lnd and tapd.
			name:        "default limit",
			wantMaxOpen: 25,
		},
		{
			name:           "configured limit",
			maxConnections: 3,
			wantMaxOpen:    3,
		},
		{
			// A single connection is enough to open the store,
			// since the migrations hand their connection back.
			name:           "single connection",
			maxConnections: 1,
			wantMaxOpen:    1,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := fixture.GetConfig()
			cfg.MaxOpenConnections = testCase.maxConnections

			store, err := NewPostgresStore(
				cfg, &chaincfg.MainNetParams,
			)
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, store.DB.Close())
			})

			stats := store.DB.Stats()
			require.Equal(
				t, testCase.wantMaxOpen,
				stats.MaxOpenConnections,
			)

			// Opening the store, including running its migrations,
			// must not leave a connection checked out.
			require.Zero(t, stats.InUse)
		})
	}
}

// TestPostgresMigrationLockTimeout tests that the migrations give up on the
// migration lock after its timeout while a competing migrator holds it, and
// that they hand their connection back without the lock once the competing
// migrator releases it.
func TestPostgresMigrationLockTimeout(t *testing.T) {
	// Let the migrations give up on the migration lock quickly.
	lockTimeout := migrate.DefaultLockTimeout
	migrate.DefaultLockTimeout = time.Second
	t.Cleanup(func() {
		migrate.DefaultLockTimeout = lockTimeout
	})

	fixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	t.Cleanup(func() {
		fixture.TearDown(t)
	})

	cfg := fixture.GetConfig()
	db, err := sql.Open("pgx", cfg.DSN(false))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	// The competing migrator holds the migration lock on a connection of
	// its own.
	ctx := context.Background()
	competitor, err := db.Conn(ctx)
	require.NoError(t, err)

	lockID, err := database.GenerateAdvisoryLockId(
		cfg.DBName, "public", postgres_migrate.DefaultMigrationsTable,
	)
	require.NoError(t, err)

	// The migrations hold the migration lock while they make sure that
	// their version table exists, and request it again after opening their
	// files. Only the second request gives up after the lock timeout, so
	// the competing migrator takes the lock when the files are opened. The
	// migrations don't apply any file then, so the files can keep their
	// sqlite syntax.
	var lockErr error
	migrationFS := &openHookFS{
		FS: sqlSchemas,
		onFirstOpen: func() {
			_, lockErr = competitor.ExecContext(
				ctx, "SELECT pg_advisory_lock($1)", lockID,
			)
		},
	}

	errChan := make(chan error, 1)
	go func() {
		errChan <- applyPostgresMigrations(db, migrationFS, cfg.DBName)
	}()

	select {
	case err := <-errChan:
		require.NoError(t, lockErr)
		require.ErrorIs(t, err, migrate.ErrLockTimeout)

	case <-time.After(30 * time.Second):
		t.Fatal("migrations didn't give up on the migration lock")
	}

	// Once the competing migrator releases the lock, the pending request of
	// the migrations gets it, and they hand their connection back without
	// it.
	_, err = competitor.ExecContext(
		ctx, "SELECT pg_advisory_unlock($1)", lockID,
	)
	require.NoError(t, err)
	require.NoError(t, competitor.Close())

	require.Eventually(t, func() bool {
		return db.Stats().InUse == 0
	}, 30*time.Second, 10*time.Millisecond)

	var numAdvisoryLocks int
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory'
	`).Scan(&numAdvisoryLocks)
	require.NoError(t, err)
	require.Zero(t, numAdvisoryLocks)
}

// openHookFS is a file system that runs a hook before it opens its first file.
type openHookFS struct {
	fs.FS

	once        sync.Once
	onFirstOpen func()
}

// Open runs the hook if no file has been opened yet and opens the named file.
//
// NOTE: This is part of the fs.FS interface.
func (o *openHookFS) Open(name string) (fs.File, error) {
	o.once.Do(o.onFirstOpen)

	return o.FS.Open(name)
}
