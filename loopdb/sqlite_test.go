package loopdb

import (
	"database/sql"
	"testing"
	"time"

	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// sqliteLockHoldTime is how long TestSqliteWriteTxWaitsForWriter holds the
// write lock in a transaction on another connection.
const sqliteLockHoldTime = 200 * time.Millisecond

// TestSqliteWriteTxLocksAtBegin checks that a write transaction takes the write
// lock when it begins. That is where SQLite applies the busy timeout, so a
// transaction that reads before it writes no longer fails with SQLITE_BUSY at
// its first write.
func TestSqliteWriteTxLocksAtBegin(t *testing.T) {
	ctx := t.Context()
	db := NewTestSqliteDB(t)

	// Take the write lock in a transaction on another connection.
	holder, err := db.BeginTx(ctx, NewSqlWriteOpts())
	require.NoError(t, err)

	err = db.Queries.WithTx(holder).UpsertLiquidityParams(ctx, []byte{1})
	require.NoError(t, err)

	// Use a dedicated connection that doesn't wait for locks, so that
	// beginning a transaction that needs the write lock fails at once.
	conn, err := db.DB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})

	_, err = conn.ExecContext(ctx, "PRAGMA busy_timeout = 0")
	require.NoError(t, err)

	// A deferred BEGIN takes no lock, so it would succeed here.
	_, err = conn.BeginTx(ctx, &sql.TxOptions{})

	var sqliteErr *sqlite.Error
	require.ErrorAs(t, err, &sqliteErr)
	require.Equal(t, sqlite3.SQLITE_BUSY, sqliteErr.Code()&0xff)

	require.NoError(t, holder.Rollback())
}

// TestSqliteWriteTxWaitsForWriter checks that a write transaction that reads
// before it writes waits for another connection's write transaction to finish
// instead of failing with SQLITE_BUSY. TestSqliteWriteTxLocksAtBegin checks
// the lock deterministically, this test checks the wait through ExecTx.
func TestSqliteWriteTxWaitsForWriter(t *testing.T) {
	ctx := t.Context()
	db := NewTestSqliteDB(t)

	require.NoError(t, db.UpsertLiquidityParams(ctx, []byte{1}))

	// Take the write lock in a transaction on another connection.
	holder, err := db.BeginTx(ctx, NewSqlWriteOpts())
	require.NoError(t, err)

	err = db.Queries.WithTx(holder).UpsertLiquidityParams(ctx, []byte{2})
	require.NoError(t, err)

	// Commit the holder after the transaction below has had time to begin.
	// If the commit happens first, the test still passes, but it doesn't
	// exercise the wait.
	var commitErr error
	committed := make(chan struct{})
	go func() {
		defer close(committed)

		time.Sleep(sqliteLockHoldTime)
		commitErr = holder.Commit()
	}()
	t.Cleanup(func() {
		<-committed
	})

	var params []byte
	err = db.ExecTx(ctx, NewSqlWriteOpts(), func(q *sqlc.Queries) error {
		var err error
		params, err = q.FetchLiquidityParams(ctx)
		if err != nil {
			return err
		}

		return q.UpsertLiquidityParams(ctx, []byte{3})
	})
	require.NoError(t, err)

	<-committed
	require.NoError(t, commitErr)

	// The transaction began only after the holder committed, so it read the
	// holder's value.
	require.Equal(t, []byte{2}, params)

	params, err = db.FetchLiquidityParams(ctx)
	require.NoError(t, err)
	require.Equal(t, []byte{3}, params)
}

// TestSqliteReadTxDoesNotWaitForWriter checks that a read-only transaction
// doesn't wait for the write lock while another connection holds it.
func TestSqliteReadTxDoesNotWaitForWriter(t *testing.T) {
	ctx := t.Context()
	db := NewTestSqliteDB(t)

	require.NoError(t, db.UpsertLiquidityParams(ctx, []byte{1}))

	// Hold the write lock in a transaction on another connection until the
	// read is done. A read-only transaction that waited for it would fail
	// with SQLITE_BUSY once the busy timeout expired.
	holder, err := db.BeginTx(ctx, NewSqlWriteOpts())
	require.NoError(t, err)

	err = db.Queries.WithTx(holder).UpsertLiquidityParams(ctx, []byte{2})
	require.NoError(t, err)

	var params []byte
	err = db.ExecTx(ctx, NewSqlReadOpts(), func(q *sqlc.Queries) error {
		var err error
		params, err = q.FetchLiquidityParams(ctx)

		return err
	})
	require.NoError(t, err)

	// The read sees the last committed value, not the holder's.
	require.Equal(t, []byte{1}, params)

	require.NoError(t, holder.Rollback())
}

// TestSqliteOpenDoesNotWaitForWriter checks that opening a database whose
// timestamps don't need to be fixed doesn't wait for the write lock, so that
// loopd can start while another connection holds it.
func TestSqliteOpenDoesNotWaitForWriter(t *testing.T) {
	ctx := t.Context()
	db := NewTestSqliteDB(t)

	// Store a swap with a valid publication deadline, which opening the
	// database checks and leaves as it is.
	swap := LoopOutContract{
		DestAddr: test.GetDestAddr(t, 0),
		SwapPublicationDeadline: time.Date(
			2025, 11, 1, 0, 0, 0, 0, time.UTC,
		),
	}
	require.NoError(t, db.CreateLoopOut(ctx, testPreimage.Hash(), &swap))

	// Hold the write lock in a transaction on another connection until the
	// database is opened again. If opening it waited for the lock, it would
	// fail with SQLITE_BUSY once the busy timeout expired.
	holder, err := db.BeginTx(ctx, NewSqlWriteOpts())
	require.NoError(t, err)

	err = db.Queries.WithTx(holder).UpsertLiquidityParams(ctx, []byte{1})
	require.NoError(t, err)

	reopened, err := NewSqliteStore(db.cfg, db.network)
	require.NoError(t, err)
	require.NoError(t, reopened.DB.Close())

	require.NoError(t, holder.Rollback())
}
