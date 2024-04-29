package loopdb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	sqlite_migrate "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // Register relevant drivers.
)

const (
	// sqliteOptionPrefix is the string prefix sqlite uses to set various
	// options. This is used in the following format:
	//   * sqliteOptionPrefix || option_name = option_value.
	sqliteOptionPrefix = "_pragma"
)

// SqliteConfig holds all the config arguments needed to interact with our
// sqlite DB.
type SqliteConfig struct {
	// SkipMigrations if true, then all the tables will be created on start
	// up if they don't already exist.
	SkipMigrations bool `long:"skipmigrations" description:"Skip applying migrations on startup."`

	// DatabaseFileName is the full file path where the database file can be
	// found.
	DatabaseFileName string `long:"dbfile" description:"The full path to the database."`
}

// SqliteSwapStore is a sqlite3 based database for the loop daemon.
type SqliteSwapStore struct {
	cfg *SqliteConfig

	*BaseDB
}

// NewSqliteStore attempts to open a new sqlite database based on the passed
// config.
func NewSqliteStore(cfg *SqliteConfig, network *chaincfg.Params) (*SqliteSwapStore, error) {
	// The set of pragma options are accepted using query options. For now
	// we only want to ensure that foreign key constraints are properly
	// enforced.
	pragmaOptions := []struct {
		name  string
		value string
	}{
		{
			name:  "foreign_keys",
			value: "on",
		},
		{
			name:  "journal_mode",
			value: "WAL",
		},
		{
			name:  "busy_timeout",
			value: "5000",
		},
		{
			// With the WAL mode, this ensures that we also do an
			// extra WAL sync after each transaction. The normal
			// sync mode skips this and gives better performance,
			// but risks durability.
			name:  "synchronous",
			value: "full",
		},
		{
			// This is used to ensure proper durability for users
			// running on Mac OS. It uses the correct fsync system
			// call to ensure items are fully flushed to disk.
			name:  "fullfsync",
			value: "true",
		},
	}
	sqliteOptions := make(url.Values)
	for _, option := range pragmaOptions {
		sqliteOptions.Add(
			sqliteOptionPrefix,
			fmt.Sprintf("%v=%v", option.name, option.value),
		)
	}

	// Construct the DSN which is just the database file name, appended
	// with the series of pragma options as a query URL string. For more
	// details on the formatting here, see the modernc.org/sqlite docs:
	// https://pkg.go.dev/modernc.org/sqlite#Driver.Open.
	dsn := fmt.Sprintf(
		"%v?%v", cfg.DatabaseFileName, sqliteOptions.Encode(),
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	if !cfg.SkipMigrations {
		// Now that the database is open, populate the database with
		// our set of schemas based on our embedded in-memory file
		// system.
		//
		// First, we'll need to open up a new migration instance for
		// our current target database: sqlite.
		driver, err := sqlite_migrate.WithInstance(
			db, &sqlite_migrate.Config{},
		)
		if err != nil {
			return nil, err
		}

		err = applyMigrations(
			sqlSchemas, driver, "sqlc/migrations", "sqlc",
		)
		if err != nil {
			return nil, err
		}
	}

	queries := sqlc.New(db)

	baseDB := &BaseDB{
		DB:      db,
		Queries: queries,
		network: network,
	}

	// Fix faulty timestamps in the database.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	err = baseDB.FixFaultyTimestamps(ctx)
	if err != nil {
		log.Errorf("Failed to fix faulty timestamps: %v", err)
		return nil, err
	}

	return &SqliteSwapStore{
		cfg:    cfg,
		BaseDB: baseDB,
	}, nil
}

// NewTestSqliteDB is a helper function that creates an SQLite database for
// testing.
func NewTestSqliteDB(t *testing.T) *SqliteSwapStore {
	t.Helper()

	t.Logf("Creating new SQLite DB for testing")

	dbFileName := filepath.Join(t.TempDir(), "tmp.db")

	sqlDB, err := NewSqliteStore(&SqliteConfig{
		DatabaseFileName: dbFileName,
		SkipMigrations:   false,
	}, &chaincfg.MainNetParams)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, sqlDB.DB.Close())
	})

	return sqlDB
}

// BaseDB is the base database struct that each implementation can embed to
// gain some common functionality.
type BaseDB struct {
	network *chaincfg.Params

	*sql.DB

	*sqlc.Queries
}

// BeginTx wraps the normal sql specific BeginTx method with the TxOptions
// interface. This interface is then mapped to the concrete sql tx options
// struct.
func (db *BaseDB) BeginTx(ctx context.Context,
	opts TxOptions) (*sql.Tx, error) {

	sqlOptions := sql.TxOptions{
		ReadOnly: opts.ReadOnly(),
	}
	return db.DB.BeginTx(ctx, &sqlOptions)
}

// ExecTx is a wrapper for txBody to abstract the creation and commit of a db
// transaction. The db transaction is embedded in a `*postgres.Queries` that
// txBody needs to use when executing each one of the queries that need to be
// applied atomically.
func (db *BaseDB) ExecTx(ctx context.Context, txOptions TxOptions,
	txBody func(*sqlc.Queries) error) error {

	// Create the db transaction.
	tx, err := db.BeginTx(ctx, txOptions)
	if err != nil {
		return err
	}

	// Rollback is safe to call even if the tx is already closed, so if
	// the tx commits successfully, this is a no-op.
	defer tx.Rollback() //nolint: errcheck

	if err := txBody(db.Queries.WithTx(tx)); err != nil {
		return err
	}

	// Commit transaction.
	if err = tx.Commit(); err != nil {
		return err
	}

	return nil
}

// FixFaultyTimestamps fixes faulty timestamps in the database, caused
// by using milliseconds instead of seconds as the publication deadline.
func (b *BaseDB) FixFaultyTimestamps(ctx context.Context) error {
	// Manually fetch all the loop out swaps.
	rows, err := b.DB.QueryContext(
		ctx, "SELECT swap_hash, swap_invoice, publication_deadline FROM loopout_swaps",
	)
	if err != nil {
		return err
	}
	defer func() {
		_ = rows.Close()
	}()

	// Parse the rows into a struct. We need to do this manually because
	// the sqlite driver will fail on faulty timestamps.
	type LoopOutRow struct {
		Hash                []byte `json:"swap_hash"`
		SwapInvoice         string `json:"swap_invoice"`
		PublicationDeadline string `json:"publication_deadline"`
	}

	var loopOutSwaps []LoopOutRow

	for rows.Next() {
		var swap LoopOutRow
		err := rows.Scan(
			&swap.Hash, &swap.SwapInvoice, &swap.PublicationDeadline,
		)
		if err != nil {
			return err
		}

		loopOutSwaps = append(loopOutSwaps, swap)
	}

	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := b.BeginTx(ctx, &SqliteTxOptions{})
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	for _, swap := range loopOutSwaps {
		// Get the year of the timestamp.
		year, err := getTimeStampYear(swap.PublicationDeadline)
		if err != nil {
			return err
		}

		// Skip if the year is not in the future.
		thisYear := time.Now().Year()
		if year > 2020 && year <= thisYear {
			continue
		}

		payReq, err := zpay32.Decode(swap.SwapInvoice, b.network)
		if err != nil {
			return err
		}
		fixedTime := payReq.Timestamp.Add(time.Minute * 30)

		// Update the faulty time to a valid time.
		_, err = tx.ExecContext(
			ctx, `
			UPDATE
			  loopout_swaps
			SET
			  publication_deadline = $1
			WHERE
			  swap_hash = $2;
			`,
			fixedTime, swap.Hash,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// TxOptions represents a set of options one can use to control what type of
// database transaction is created. Transaction can whether be read or write.
type TxOptions interface {
	// ReadOnly returns true if the transaction should be read only.
	ReadOnly() bool
}

// SqliteTxOptions defines the set of db txn options the KeyStore
// understands.
type SqliteTxOptions struct {
	// readOnly governs if a read only transaction is needed or not.
	readOnly bool
}

// NewSqlReadOpts returns a new KeyStoreTxOptions instance triggers a read
// transaction.
func NewSqlReadOpts() *SqliteTxOptions {
	return &SqliteTxOptions{
		readOnly: true,
	}
}

// ReadOnly returns true if the transaction should be read only.
//
// NOTE: This implements the TxOptions interface.
func (r *SqliteTxOptions) ReadOnly() bool {
	return r.readOnly
}

// getTimeStampYear returns the year of a timestamp string.
func getTimeStampYear(dateTimeStr string) (int, error) {
	parts := strings.Split(dateTimeStr, "-")
	if len(parts) < 1 {
		return 0, fmt.Errorf("invalid timestamp format: %v",
			dateTimeStr)
	}

	year, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("unable to parse year: %v", err)
	}

	return year, nil
}
