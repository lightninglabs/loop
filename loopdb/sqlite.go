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
		ctx, "SELECT swap_hash, publication_deadline FROM loopout_swaps",
	)
	if err != nil {
		return err
	}

	// Parse the rows into a struct. We need to do this manually because
	// the sqlite driver will fail on faulty timestamps.
	type LoopOutRow struct {
		Hash                []byte `json:"swap_hash"`
		PublicationDeadline string `json:"publication_deadline"`
	}

	var loopOutSwaps []LoopOutRow

	for rows.Next() {
		var swap LoopOutRow
		err := rows.Scan(
			&swap.Hash, &swap.PublicationDeadline,
		)
		if err != nil {
			return err
		}

		loopOutSwaps = append(loopOutSwaps, swap)
	}

	tx, err := b.BeginTx(ctx, &SqliteTxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint: errcheck

	for _, swap := range loopOutSwaps {
		faultyTime, err := parseTimeStamp(swap.PublicationDeadline)
		if err != nil {
			return err
		}

		// Skip if the time is not faulty.
		if !isMilisecondsTime(faultyTime.Unix()) {
			continue
		}

		// Update the faulty time to a valid time.
		secs := faultyTime.Unix() / 1000
		correctTime := time.Unix(secs, 0)
		_, err = tx.ExecContext(
			ctx, `
			UPDATE
			  loopout_swaps
			SET
			  publication_deadline = $1
			WHERE
			  swap_hash = $2;
			`,
			correctTime, swap.Hash,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// TxOptions represents a set of options one can use to control what type of
// database transaction is created. Transaction can wither be read or write.
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

// NewKeyStoreReadOpts returns a new KeyStoreTxOptions instance triggers a read
// transaction.
func NewSqlReadOpts() *SqliteTxOptions {
	return &SqliteTxOptions{
		readOnly: true,
	}
}

// ReadOnly returns true if the transaction should be read only.
//
// NOTE: This implements the TxOptions
func (r *SqliteTxOptions) ReadOnly() bool {
	return r.readOnly
}

// parseTimeStamp tries to parse a timestamp string with both the
// parseSqliteTimeStamp and parsePostgresTimeStamp functions.
// If both fail, it returns an error.
func parseTimeStamp(dateTimeStr string) (time.Time, error) {
	t, err := parseSqliteTimeStamp(dateTimeStr)
	if err != nil {
		t, err = parsePostgresTimeStamp(dateTimeStr)
		if err != nil {
			return time.Time{}, err
		}
	}

	return t, nil
}

// parseSqliteTimeStamp parses a timestamp string in the format of
// "YYYY-MM-DD HH:MM:SS +0000 UTC" and returns a time.Time value.
// NOTE: we can't use time.Parse() because it doesn't support having years
// with more than 4 digits.
func parseSqliteTimeStamp(dateTimeStr string) (time.Time, error) {
	// Split the date and time parts.
	parts := strings.Fields(strings.TrimSpace(dateTimeStr))
	if len(parts) < 2 {
		return time.Time{}, fmt.Errorf("invalid timestamp format: %v",
			dateTimeStr)
	}

	datePart, timePart := parts[0], parts[1]

	return parseTimeParts(datePart, timePart)
}

// parseSqliteTimeStamp parses a timestamp string in the format of
// "YYYY-MM-DDTHH:MM:SSZ" and returns a time.Time value.
// NOTE: we can't use time.Parse() because it doesn't support having years
// with more than 4 digits.
func parsePostgresTimeStamp(dateTimeStr string) (time.Time, error) {
	// Split the date and time parts.
	parts := strings.Split(dateTimeStr, "T")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("invalid timestamp format: %v",
			dateTimeStr)
	}

	datePart, timePart := parts[0], strings.TrimSuffix(parts[1], "Z")

	return parseTimeParts(datePart, timePart)
}

// parseTimeParts takes a datePart string in the format of "YYYY-MM-DD" and
// a timePart string in the format of "HH:MM:SS" and returns a time.Time value.
func parseTimeParts(datePart, timePart string) (time.Time, error) {
	// Parse the date.
	dateParts := strings.Split(datePart, "-")
	if len(dateParts) != 3 {
		return time.Time{}, fmt.Errorf("invalid date format: %v",
			datePart)
	}

	year, err := strconv.Atoi(dateParts[0])
	if err != nil {
		return time.Time{}, err
	}

	month, err := strconv.Atoi(dateParts[1])
	if err != nil {
		return time.Time{}, err
	}

	day, err := strconv.Atoi(dateParts[2])
	if err != nil {
		return time.Time{}, err
	}

	// Parse the time.
	timeParts := strings.Split(timePart, ":")
	if len(timeParts) != 3 {
		return time.Time{}, fmt.Errorf("invalid time format: %v",
			timePart)
	}

	hour, err := strconv.Atoi(timeParts[0])
	if err != nil {
		return time.Time{}, err
	}

	minute, err := strconv.Atoi(timeParts[1])
	if err != nil {
		return time.Time{}, err
	}

	// Parse the seconds and ignore the fractional part.
	secondParts := strings.Split(timeParts[2], ".")

	second, err := strconv.Atoi(secondParts[0])
	if err != nil {
		return time.Time{}, err
	}

	// Construct a time.Time value.
	return time.Date(
		year, time.Month(month), day, hour, minute, second, 0, time.UTC,
	), nil
}

// isMilisecondsTime returns true if the unix timestamp is likely in
// milliseconds.
func isMilisecondsTime(unixTimestamp int64) bool {
	length := len(fmt.Sprintf("%d", unixTimestamp))
	if length >= 13 {
		// Likely a millisecond timestamp
		return true
	} else {
		// Likely a second timestamp
		return false
	}
}
