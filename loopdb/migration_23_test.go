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

// TestMigration23BackfillsLoopInChangeAddress verifies that existing
// fractional loop-ins remain tied to the legacy root address they used for
// change before per-swap change addresses were introduced.
func TestMigration23BackfillsLoopInChangeAddress(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migration-23.db")
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

	require.NoError(t, schemaMigrate.Migrate(22))

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

	// Add a newer address to prove that the migration selects the original
	// legacy root rather than whichever address was inserted most recently.
	_, err = db.Exec(`
		INSERT INTO static_addresses (
			client_pubkey, server_pubkey, expiry, client_key_family,
			client_key_index, pkscript, protocol_version,
			initiation_height
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		[]byte{4}, []byte{5}, 144, 1, 3, []byte{6}, 0, 101,
	)
	require.NoError(t, err)

	swapHash := []byte{7}
	_, err = db.Exec(`
		INSERT INTO static_address_swaps (
			swap_hash, swap_invoice, payment_timeout_seconds,
			quoted_swap_fee_satoshis, deposit_outpoints,
			htlc_tx_fee_rate_sat_kw, htlc_timeout_sweep_address,
			selected_amount
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		swapHash, "invoice", 3600, 1000, "txid:0", 2500,
		"bcrt1qexample", 60_000,
	)
	require.NoError(t, err)

	require.NoError(t, schemaMigrate.Migrate(23))

	var changeAddressID int64
	err = db.QueryRow(`
		SELECT change_static_address_id
		FROM static_address_swaps
		WHERE swap_hash = ?`, swapHash,
	).Scan(&changeAddressID)
	require.NoError(t, err)
	require.Equal(t, legacyAddressID, changeAddressID)
}
