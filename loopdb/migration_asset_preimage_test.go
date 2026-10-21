package loopdb

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNullableAssetPreimageMigration upgrades populated Bitcoin tables and
// retains foreign keys while allowing several unknown server-owned secrets.
func TestNullableAssetPreimageMigration(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(t.Context(), "PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	files, err := fs.Glob(sqlSchemas, "sqlc/migrations/*.up.sql")
	require.NoError(t, err)
	for _, name := range files {
		if strings.Contains(name, "000023_") {
			continue
		}
		raw, err := sqlSchemas.ReadFile(name)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), string(raw))
		require.NoError(t, err, name)
	}
	insert := `INSERT INTO swaps (swap_hash, preimage, initiation_time,
		amount_requested, cltv_expiry, max_miner_fee, max_swap_fee,
		initiation_height, protocol_version, label)
		VALUES (?, ?, CURRENT_TIMESTAMP, 100, 200, 1, 2, 3, 4, '')`
	_, err = db.ExecContext(t.Context(), insert, []byte{1}, []byte{2})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO loopin_swaps
		(swap_hash, htlc_conf_target, external_htlc) VALUES (?, 1, false)`,
		[]byte{1})
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), sqliteNullablePreimage)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var preimage []byte
	err = db.QueryRowContext(t.Context(),
		"SELECT preimage FROM swaps WHERE swap_hash = ?", []byte{1},
	).Scan(&preimage)
	require.NoError(t, err)
	require.Equal(t, []byte{2}, preimage)
	for _, hash := range []byte{3, 4} {
		_, err = db.ExecContext(t.Context(), insert, []byte{hash}, nil)
		require.NoError(t, err)
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO loopin_swaps
		(swap_hash, htlc_conf_target, external_htlc) VALUES (?, 1, false)`,
		[]byte{9})
	require.Error(t, err)
	_, err = db.ExecContext(t.Context(), insert, []byte{5}, []byte{2})
	require.Error(t, err, "known preimages remain unique")
}
