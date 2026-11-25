package loopdb

import (
	"database/sql"
	"io/fs"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	postgres_migrate "github.com/golang-migrate/migrate/v4/database/postgres"
	sqlite_migrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/httpfs"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigration24BackfillsDepositAddressOwnership checks legacy ownership and
// rejects ambiguous backfills. It uses PostgreSQL with the test_db_postgres tag.
func TestMigration24BackfillsDepositAddressOwnership(t *testing.T) {
	tests := []struct {
		name      string
		addresses int
		deposits  int
		wantErr   bool
	}{
		{name: "empty database"},
		{name: "unused address", addresses: 1},
		{name: "unused addresses", addresses: 2},
		{name: "shared legacy address", addresses: 1, deposits: 2},
		{name: "missing address", deposits: 1, wantErr: true},
		{name: "ambiguous address", addresses: 2, deposits: 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, schemaMigrate := migration24TestDB(t)
			require.NoError(t, schemaMigrate.Migrate(23))

			// Explicit IDs ensure the backfill uses the stored identity.
			const legacyAddressID = 42
			insertAddress := func(id int) {
				_, err := db.ExecContext(t.Context(), `
					INSERT INTO static_addresses (
						id, client_pubkey, server_pubkey, expiry,
						client_key_family, client_key_index, pkscript,
						protocol_version, initiation_height
					) VALUES ($1, $2, $3, 144, 1, 2, $4, 0, 100)`,
					id, []byte{1}, []byte{2}, []byte{byte(id)},
				)
				require.NoError(t, err)
			}
			for i := range test.addresses {
				insertAddress(legacyAddressID + i)
			}
			for i := range test.deposits {
				_, err := db.ExecContext(t.Context(), `
					INSERT INTO deposits (
						deposit_id, tx_hash, out_index, amount,
						confirmation_height, timeout_sweep_pk_script
					) VALUES ($1, $2, $3, 100000, 200, $4)`,
					[]byte{byte(i)}, make([]byte, 32), i, []byte{4},
				)
				require.NoError(t, err)
			}

			err := schemaMigrate.Migrate(24)
			if test.wantErr {
				require.ErrorContains(
					t, err, "migration_24_requires_one_legacy_address",
				)

				// A rejected migration must leave the old schema and data
				// intact. The framework marks the failed version dirty.
				version, dirty, err := schemaMigrate.Version()
				require.NoError(t, err)
				require.EqualValues(t, 24, version)
				require.True(t, dirty)

				var addressID int64
				err = db.QueryRowContext(t.Context(),
					"SELECT static_address_id FROM deposits LIMIT 1",
				).Scan(&addressID)
				require.ErrorContains(t, err, "static_address_id")

				var count int
				err = db.QueryRowContext(t.Context(),
					"SELECT COUNT(*) FROM deposits").Scan(&count)
				require.NoError(t, err)
				require.Equal(t, test.deposits, count)
				err = db.QueryRowContext(t.Context(),
					"SELECT COUNT(*) FROM static_addresses").Scan(&count)
				require.NoError(t, err)
				require.Equal(t, test.addresses, count)

				// Simulate operator repair before resetting the version.
				// Retrying also proves that the guard table rolled back.
				if test.addresses == 0 {
					insertAddress(legacyAddressID)
				} else {
					_, err = db.ExecContext(t.Context(),
						"DELETE FROM static_addresses WHERE id <> $1",
						legacyAddressID)
					require.NoError(t, err)
				}
				require.NoError(t, schemaMigrate.Force(23))
				require.NoError(t, schemaMigrate.Migrate(24))
			} else {
				require.NoError(t, err)
			}

			var assigned int
			err = db.QueryRowContext(t.Context(), `
				SELECT COUNT(*) FROM deposits WHERE static_address_id = $1`,
				legacyAddressID).Scan(&assigned)
			require.NoError(t, err)
			require.Equal(t, test.deposits, assigned)

			// The guard applies only to legacy data: multiple addresses must
			// be allowed after this migration, including on later upgrades.
			insertAddress(100)
			insertAddress(101)
			require.NoError(t, schemaMigrate.Up())
		})
	}
}

// migration24TestDB opens an unmigrated database using the production migration
// driver and SQL transformations for the selected test backend.
func migration24TestDB(t *testing.T) (*sql.DB, *migrate.Migrate) {
	return migration24TestDBWithPrivileges(t, false)
}

// migration24TestDBWithPrivileges can use a role with schema CREATE permission
// but no database TEMPORARY permission, as in a restricted production setup.
func migration24TestDBWithPrivileges(t *testing.T,
	withoutTemp bool) (*sql.DB, *migrate.Migrate) {

	t.Helper()

	var (
		db       *sql.DB
		driver   database.Driver
		schemaFS fs.FS = sqlSchemas
		err      error
	)
	if testDBType == "postgres" {
		fixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
		t.Cleanup(func() { fixture.TearDown(t) })
		cfg := fixture.GetConfig()
		if withoutTemp {
			_, err := fixture.db.ExecContext(t.Context(), `
				REVOKE TEMPORARY ON DATABASE test FROM PUBLIC;
				CREATE ROLE loop_migrator LOGIN PASSWORD 'migration-test';
				GRANT CONNECT ON DATABASE test TO loop_migrator;
				GRANT USAGE, CREATE ON SCHEMA public TO loop_migrator;
			`)
			require.NoError(t, err)
			cfg.User = "loop_migrator"
			cfg.Password = "migration-test"
		}
		db, err = sql.Open("pgx", cfg.DSN(false))
		require.NoError(t, err)
		driver, err = postgres_migrate.WithInstance(
			db, &postgres_migrate.Config{},
		)
		schemaFS = newReplacerFS(sqlSchemas, map[string]string{
			"BLOB":                "BYTEA",
			"INTEGER PRIMARY KEY": "SERIAL PRIMARY KEY",
			txidSqlite:            txidPostgres,
		})
	} else {
		db, err = sql.Open("sqlite",
			filepath.Join(t.TempDir(), "migration-24.db"))
		require.NoError(t, err)
		driver, err = sqlite_migrate.WithInstance(
			db, &sqlite_migrate.Config{},
		)
	}
	require.NoError(t, err)

	source, err := httpfs.New(http.FS(schemaFS), "sqlc/migrations")
	require.NoError(t, err)
	schemaMigrate, err := migrate.NewWithInstance(
		"migrations", source, "sqlc", driver)
	require.NoError(t, err)
	t.Cleanup(func() {
		sourceErr, databaseErr := schemaMigrate.Close()
		require.NoError(t, sourceErr)
		require.NoError(t, databaseErr)
	})
	return db, schemaMigrate
}

// TestMigration24WithoutTemporaryPrivilege verifies valid legacy databases can
// upgrade using only existing schema permissions, without database TEMPORARY.
func TestMigration24WithoutTemporaryPrivilege(t *testing.T) {
	if testDBType != "postgres" {
		t.Skip("requires test_db_postgres")
	}

	for _, withDeposit := range []bool{false, true} {
		name := "empty database"
		if withDeposit {
			name = "legacy deposit"
		}
		t.Run(name, func(t *testing.T) {
			db, schemaMigrate := migration24TestDBWithPrivileges(t, true)
			require.NoError(t, schemaMigrate.Migrate(23))

			var hasTemporary bool
			err := db.QueryRowContext(t.Context(), `SELECT
				has_database_privilege(current_user, current_database(),
				'TEMPORARY')`).Scan(&hasTemporary)
			require.NoError(t, err)
			require.False(t, hasTemporary)
			if withDeposit {
				_, err := db.ExecContext(t.Context(), `
					INSERT INTO static_addresses (
						client_pubkey, server_pubkey, expiry,
						client_key_family, client_key_index, pkscript,
						protocol_version, initiation_height
					) VALUES ('\x01', '\x02', 144, 42060, 0, '\x03', 0, 100);
					INSERT INTO deposits (
						deposit_id, tx_hash, out_index, amount,
						confirmation_height, timeout_sweep_pk_script
					) VALUES ('\x01', '\x02', 0, 100000, 200, '\x04');
				`)
				require.NoError(t, err)
			}

			require.NoError(t, schemaMigrate.Migrate(24))
			version, dirty, err := schemaMigrate.Version()
			require.NoError(t, err)
			require.EqualValues(t, 24, version)
			require.False(t, dirty)
			if withDeposit {
				var addressID int64
				err := db.QueryRowContext(t.Context(), `SELECT
					static_address_id FROM deposits`).Scan(&addressID)
				require.NoError(t, err)
				require.Positive(t, addressID)
			}
			require.NoError(t, schemaMigrate.Up())
		})
	}
}
