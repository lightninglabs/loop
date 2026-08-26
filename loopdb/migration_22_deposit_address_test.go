package loopdb

import (
	"context"
	"database/sql"
	"io/fs"
	"net/http"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/golang-migrate/migrate/v4"
	sqlite_migrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/httpfs"
	"github.com/stretchr/testify/require"
)

// TestDepositAddressBackfill verifies that migration 22 only assigns legacy
// deposits when their static-address owner is unambiguous.
func TestDepositAddressBackfill(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		addressCount int
		wantOwner    bool
	}{
		{name: "no address", addressCount: 0},
		{name: "one address", addressCount: 1, wantOwner: true},
		{name: "multiple addresses", addressCount: 2},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			db, err := sql.Open(
				"sqlite", filepath.Join(t.TempDir(), "loop.db"),
			)
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, db.Close())
			})

			ctx := context.Background()
			_, err = db.ExecContext(ctx, `
				CREATE TABLE static_addresses (id INTEGER PRIMARY KEY);
				CREATE TABLE deposits (id INTEGER PRIMARY KEY);`)
			require.NoError(t, err)

			migrationSQL, err := fs.ReadFile(
				sqlSchemas,
				"sqlc/migrations/000022_deposit_static_address_id.up.sql",
			)
			require.NoError(t, err)
			migrationFS := fstest.MapFS{
				"migrations/000001_deposit_address.up.sql": {
					Data: migrationSQL,
				},
			}

			driver, err := sqlite_migrate.WithInstance(
				db, &sqlite_migrate.Config{},
			)
			require.NoError(t, err)
			source, err := httpfs.New(
				http.FS(migrationFS), "migrations",
			)
			require.NoError(t, err)
			migrator, err := migrate.NewWithInstance(
				"migrations", source, "sqlc", driver,
			)
			require.NoError(t, err)

			for i := 0; i < testCase.addressCount; i++ {
				_, err := db.ExecContext(ctx,
					"INSERT INTO static_addresses (id) VALUES (?)",
					i+1,
				)
				require.NoError(t, err)
			}

			_, err = db.ExecContext(ctx,
				"INSERT INTO deposits (id) VALUES (1)",
			)
			require.NoError(t, err)

			require.NoError(t, migrator.Up())

			var owner sql.NullInt64
			err = db.QueryRowContext(ctx,
				"SELECT static_address_id FROM deposits",
			).Scan(&owner)
			require.NoError(t, err)
			require.Equal(t, testCase.wantOwner, owner.Valid)
			if testCase.wantOwner {
				require.EqualValues(t, 1, owner.Int64)
			}
		})
	}
}
