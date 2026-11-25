package loopdb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigration26FundingTransactions keeps existing swaps without snapshots
// and stores binary snapshots on both SQLite and PostgreSQL.
func TestMigration26FundingTransactions(t *testing.T) {
	db, schemaMigrate := migration24TestDB(t)
	require.NoError(t, schemaMigrate.Migrate(25))
	ctx := t.Context()
	swapHash := []byte{1}
	_, err := db.ExecContext(ctx, `INSERT INTO static_address_swaps (
		swap_hash, swap_invoice, payment_timeout_seconds,
		quoted_swap_fee_satoshis, deposit_outpoints,
		htlc_tx_fee_rate_sat_kw, htlc_timeout_sweep_address
	) VALUES ($1, 'invoice', 60, 1000, 'txid:0', 2500, 'address')`,
		swapHash)
	require.NoError(t, err)
	require.NoError(t, schemaMigrate.Migrate(26))
	var snapshot []byte
	err = db.QueryRowContext(ctx, `SELECT funding_txs
		FROM static_address_swaps WHERE swap_hash = $1`,
		swapHash).Scan(&snapshot)
	require.NoError(t, err)
	require.Nil(t, snapshot)

	want := []byte(`["AQID"]`)
	_, err = db.ExecContext(ctx, `UPDATE static_address_swaps
		SET funding_txs = $1 WHERE swap_hash = $2`, want, swapHash)
	require.NoError(t, err)
	err = db.QueryRowContext(ctx, `SELECT funding_txs
		FROM static_address_swaps WHERE swap_hash = $1`,
		swapHash).Scan(&snapshot)
	require.NoError(t, err)
	require.Equal(t, want, snapshot)

	version, dirty, err := schemaMigrate.Version()
	require.NoError(t, err)
	require.EqualValues(t, 26, version)
	require.False(t, dirty)
}
