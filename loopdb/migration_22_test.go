package loopdb

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	sqlite_migrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/httpfs"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigration22BackfillsDepositAddressOwnership verifies that the ownership
// migration durably links pre-multi-address deposits to the legacy root static
// address.
func TestMigration22BackfillsDepositAddressOwnership(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migration-22.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)

	driver, err := sqlite_migrate.WithInstance(
		db, &sqlite_migrate.Config{},
	)
	require.NoError(t, err)

	source, err := httpfs.New(
		http.FS(sqlSchemas), "sqlc/migrations",
	)
	require.NoError(t, err)

	schemaMigrate, err := migrate.NewWithInstance(
		"migrations", source, "sqlc", driver,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		sourceErr, databaseErr := schemaMigrate.Close()
		require.NoError(t, sourceErr)
		require.NoError(t, databaseErr)
	})

	require.NoError(t, schemaMigrate.Migrate(21))

	result, err := db.Exec(`
		INSERT INTO static_addresses (
			client_pubkey, server_pubkey, expiry, client_key_family,
			client_key_index, pkscript, protocol_version,
			initiation_height
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		[]byte{1}, []byte{2}, 144, 1, 2, []byte{3}, 0, 100,
	)
	require.NoError(t, err)
	legacyAddressID, err := result.LastInsertId()
	require.NoError(t, err)

	_, err = db.Exec(`
		INSERT INTO deposits (
			deposit_id, tx_hash, out_index, amount,
			confirmation_height, timeout_sweep_pk_script
		) VALUES (?, ?, ?, ?, ?, ?)`,
		make([]byte, 32), make([]byte, 32), 0, 100_000, 200,
		[]byte{4},
	)
	require.NoError(t, err)

	require.NoError(t, schemaMigrate.Migrate(22))

	var staticAddressID int64
	err = db.QueryRow(
		"SELECT static_address_id FROM deposits",
	).Scan(&staticAddressID)
	require.NoError(t, err)
	require.Equal(t, legacyAddressID, staticAddressID)
}
