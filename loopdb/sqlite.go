package loopdb

import (
	"context"
	"database/sql"
	"errors"
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
	defer func() {
		_ = rows.Close()
	}()

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
		if year <= thisYear {
			continue
		}

		fixedTime, err := fixTimeStamp(swap.PublicationDeadline)
		if err != nil {
			return err
		}

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

// fixTimeStamp tries to parse a timestamp string with both the
// parseSqliteTimeStamp and parsePostgresTimeStamp functions.
// If both fail, it returns an error.
func fixTimeStamp(dateTimeStr string) (time.Time, error) {
	year, err := getTimeStampYear(dateTimeStr)
	if err != nil {
		return time.Time{}, err
	}

	// If the year is in the future. It was a faulty timestamp.
	thisYear := time.Now().Year()
	if year > thisYear {
		dateTimeStr = strings.Replace(
			dateTimeStr,
			fmt.Sprintf("%d", year),
			fmt.Sprintf("%d", thisYear),
			1,
		)
	}

	// If the year is a leap year and the date is 29th of February, we
	// need to change it to 28th of February. Otherwise, the time.Parse
	// function will fail, as a non-leap year cannot have 29th of February.
	day, month, err := extractDayAndMonth(dateTimeStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("unable to parse timestamp day "+
			"and month %v: %v", dateTimeStr, err)
	}

	if !isLeapYear(thisYear) &&
		month == 2 && day == 29 {

		dateTimeStr = strings.Replace(
			dateTimeStr,
			fmt.Sprintf("%d-02-29", thisYear),
			fmt.Sprintf("%d-02-28", thisYear),
			1,
		)
	}

	parsedTime, err := parseLayouts(defaultLayouts(), dateTimeStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("unable to parse timestamp %v: %v",
			dateTimeStr, err)
	}

	return parsedTime.UTC(), nil
}

// parseLayouts parses time based on a list of provided layouts.
// If layouts is empty list or nil, the error with unknown layout will be returned.
func parseLayouts(layouts []string, dateTime string) (time.Time, error) {
	for _, layout := range layouts {
		parsedTime, err := time.Parse(layout, dateTime)
		if err == nil {
			return parsedTime, nil
		}
	}

	return time.Time{}, errors.New("unknown layout")
}

// defaultLayouts returns a default list of ALL supported layouts.
// This function returns new copy of a slice.
func defaultLayouts() []string {
	return []string{
		"2006-01-02 15:04:05.99999 -0700 MST", // Custom sqlite layout.
		time.RFC3339Nano,
		time.RFC3339,
		time.RFC1123Z,
		time.RFC1123,
		time.RFC850,
		time.RFC822Z,
		time.RFC822,
		time.Layout,
		time.RubyDate,
		time.UnixDate,
		time.ANSIC,
		time.StampNano,
		time.StampMicro,
		time.StampMilli,
		time.Stamp,
		time.Kitchen,
	}
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

// extractDayAndMonth extracts the day and month from a date string.
func extractDayAndMonth(dateStr string) (int, int, error) {
	// Split the date string into parts using various delimiters.
	parts := strings.FieldsFunc(dateStr, func(r rune) bool {
		return r == '-' || r == ' ' || r == 'T' || r == ':' || r == '+' || r == 'Z'
	})

	if len(parts) < 3 {
		return 0, 0, fmt.Errorf("Invalid date format: %s", dateStr)
	}

	// Extract year, month, and day from the parts.
	_, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}

	month, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}

	day, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, err
	}

	return day, month, nil
}

// isLeapYear returns true if the year is a leap year.
func isLeapYear(year int) bool {
	return (year%4 == 0 && year%100 != 0) || (year%400 == 0)
}
