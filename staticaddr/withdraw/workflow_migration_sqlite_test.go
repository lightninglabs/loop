package withdraw

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/golang-migrate/migrate/v4"
	sqlite_migrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	_ "github.com/golang-migrate/migrate/v4/source/file" // Register file migrations.
	fsmv2 "github.com/lightninglabs/loop/fsm/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/stretchr/testify/require"
)

func TestWithdrawalWorkflowPhysicalReopen(t *testing.T) {
	ctx := t.Context()
	cfg := &loopdb.SqliteConfig{DatabaseFileName: filepath.Join(t.TempDir(), "withdrawal.sqlite")}
	db, err := loopdb.NewSqliteStore(cfg, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	f := newPersistenceFixtureDB(t, db.BaseDB, 1)
	data, tx := f.intent(t, f.inputs, 100)
	require.NoError(t, f.store.WorkflowStore().Create(ctx, persistenceRecord(t, data, WorkflowSigning, 1)))
	signed, err := encodeWorkflowTx(tx)
	require.NoError(t, err)
	data.Attempts = []WorkflowAttempt{{ID: 1, Tx: signed, TxID: tx.TxHash().String()}}
	committed := persistenceRecord(t, data, WorkflowPublishPending, 2)
	require.NoError(t, f.store.WorkflowStore().CompareAndSwap(ctx, committed, 1))
	require.NoError(t, db.Close())

	reopened, err := loopdb.NewSqliteStore(cfg, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	store := NewSqlStore(loopdb.NewTypedStore[Querier](reopened), nil).WorkflowStore()
	loaded, err := store.Load(ctx, data.ID)
	require.NoError(t, err)
	require.Equal(t, committed, loaded)
	active, err := store.ListActive(ctx)
	require.NoError(t, err)
	require.Equal(t, []fsmv2.Record{committed}, active)
	owner, err := reopened.GetWithdrawalWorkflowClaim(ctx, f.inputs[0].ID[:])
	require.NoError(t, err)
	require.Equal(t, data.ID, owner)
	row, err := reopened.GetDeposit(ctx, f.inputs[0].ID[:])
	require.NoError(t, err)
	require.True(t, row.FinalizedWithdrawalTx.Valid)
	history, err := reopened.GetWithdrawalWorkflowUpdates(ctx, data.ID)
	require.NoError(t, err)
	require.Len(t, history, 2)
}

// TestWithdrawalSchemaUpgrade23 preserves a real version-23 legacy withdrawal
// database across ordinary daemon migration to version 24 and backfills v2.
func TestWithdrawalSchemaUpgrade23(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "version23.sqlite")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	driver, err := sqlite_migrate.WithInstance(raw, &sqlite_migrate.Config{})
	require.NoError(t, err)
	dir, err := filepath.Abs("../../loopdb/sqlc/migrations")
	require.NoError(t, err)
	migration, err := migrate.NewWithDatabaseInstance("file://"+dir, "sqlite", driver)
	require.NoError(t, err)
	require.NoError(t, migration.Migrate(23))
	sourceErr, dbErr := migration.Close()
	require.NoError(t, sourceErr)
	require.NoError(t, dbErr)

	old, err := loopdb.NewSqliteStore(&loopdb.SqliteConfig{
		DatabaseFileName: path, SkipMigrations: true,
	}, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	f := newPersistenceFixtureDB(t, old.BaseDB, 1)
	_, tx := f.intent(t, f.inputs, 101)
	require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs))
	require.NoError(t, f.store.UpdateWithdrawal(ctx, f.inputs, tx, 120, f.script))
	legacy, err := old.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Len(t, legacy, 1)
	require.NoError(t, old.Close())

	upgraded, err := loopdb.NewSqliteStore(&loopdb.SqliteConfig{
		DatabaseFileName: path,
	}, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	after, err := upgraded.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Equal(t, legacy, after)
	store := NewSqlStore(loopdb.NewTypedStore[Querier](upgraded), nil)
	require.NoError(t, store.WorkflowStore().MigrateLegacy(ctx))
	listings, err := store.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Len(t, listings, 1)
	require.Equal(t, tx.TxHash(), listings[0].TxID)
	require.EqualValues(t, 120, listings[0].ConfirmationHeight)
	after, err = upgraded.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Equal(t, legacy, after)
}
