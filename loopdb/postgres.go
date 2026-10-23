package loopdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/golang-migrate/migrate/v4"
	postgres_migrate "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/stretchr/testify/require"
)

const (
	dsnTemplate = "postgres://%v:%v@%v:%d/%v?sslmode=%v"

	// defaultMaxConns is the maximum number of open connections to the
	// database server if no limit is configured. lnd and tapd use the same
	// default.
	defaultMaxConns = 25
)

var (
	// DefaultPostgresFixtureLifetime is the default maximum time a Postgres
	// test fixture is being kept alive. After that time the docker
	// container will be terminated forcefully, even if the tests aren't
	// fully executed yet. So this time needs to be chosen correctly to be
	// longer than the longest expected individual test run time.
	DefaultPostgresFixtureLifetime = 10 * time.Minute
)

// PostgresConfig holds the postgres database configuration.
type PostgresConfig struct {
	SkipMigrations     bool   `long:"skipmigrations" description:"Skip applying migrations on startup."`
	Host               string `long:"host" description:"Database server hostname."`
	Port               int    `long:"port" description:"Database server port."`
	User               string `long:"user" description:"Database user."`
	Password           string `long:"password" description:"Database user's password."` //nolint:gosec
	DBName             string `long:"dbname" description:"Database name to use."`
	MaxOpenConnections int32  `long:"maxconnections" description:"Maximum number of open connections to the database server. Defaults to 25 if unset."`
	RequireSSL         bool   `long:"requiressl" description:"Whether to require using SSL (mode: require) when connecting to the server."`
}

// DSN returns the data source name used to connect to the database.
func (s *PostgresConfig) DSN(hidePassword bool) string {
	var sslMode = "disable"
	if s.RequireSSL {
		sslMode = "require"
	}

	password := s.Password
	if hidePassword {
		// Placeholder used for logging the DSN safely.
		password = "****"
	}

	return fmt.Sprintf(dsnTemplate, s.User, password, s.Host, s.Port,
		s.DBName, sslMode)
}

// PostgresStore is a database store implementation that uses a Postgres
// backend.
type PostgresStore struct {
	*BaseDB

	cfg *PostgresConfig
}

// In migration of sweeps table from outpoint_txid and outpoint_index to
// outpoint we need to reverse the order of bytes in outpoint_txid and to
// convert it to hex. This is done differently in sqlite and postgres.
//
// Changes from sqlite to postgres:
//   - substr(blob, ...) -> get_byte(blob, index)
//   - group_concat -> string_agg
//   - 1-based indexing (32+1-i) -> 0-based (32 - i)
//   - to_hex() + lpad(..., 2, '0') ensures each byte is two-digit hex
const (
	txidSqlite   = "group_concat(hex(substr(outpoint_txid,32+1-i,1)),'')"
	txidPostgres = "string_agg(lpad(to_hex(get_byte(outpoint_txid, " +
		"32 - i)), 2, '0'), '')"
)

// NewPostgresStore creates a new store that is backed by a Postgres database
// backend.
func NewPostgresStore(cfg *PostgresConfig,
	network *chaincfg.Params) (*PostgresStore, error) {

	log.Infof("Using SQL database '%s'", cfg.DSN(true))

	rawDb, err := sql.Open("pgx", cfg.DSN(false))
	if err != nil {
		return nil, err
	}

	// Bound the connection pool. Without a limit, concurrent queries open
	// as many connections as they need, which can exceed the number of
	// connections that the database server accepts.
	maxConns := defaultMaxConns
	if cfg.MaxOpenConnections > 0 {
		maxConns = int(cfg.MaxOpenConnections)
	}
	rawDb.SetMaxOpenConns(maxConns)

	if !cfg.SkipMigrations {
		// Now that the database is open, populate the database with
		// our set of schemas based on our embedded in-memory file
		// system.
		postgresFS := newReplacerFS(sqlSchemas, map[string]string{
			"BLOB":                "BYTEA",
			"INTEGER PRIMARY KEY": "SERIAL PRIMARY KEY",
			txidSqlite:            txidPostgres,
		})

		err = applyPostgresMigrations(rawDb, postgresFS, cfg.DBName)
		if err != nil {
			return nil, err
		}
	}

	queries := sqlc.New(rawDb)

	baseDB := &BaseDB{
		DB:      rawDb,
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

	return &PostgresStore{
		cfg:    cfg,
		BaseDB: baseDB,
	}, nil
}

// applyPostgresMigrations applies the migrations of the passed file system on a
// connection of the database pool and hands the connection back once they are
// done. A migration driver created by postgres_migrate.WithInstance keeps its
// connection checked out until it is closed, which would also close the
// database.
func applyPostgresMigrations(db *sql.DB, migrationFS fs.FS,
	dbName string) error {

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}

	// Open up a new migration instance for our current target database:
	// postgres. It only holds session-level advisory locks while it
	// migrates, so the connection can be reused afterwards.
	driver, err := postgres_migrate.WithConnection(
		ctx, conn, &postgres_migrate.Config{},
	)
	if err != nil {
		_ = conn.Close()

		return err
	}

	err = applyMigrations(migrationFS, driver, "sqlc/migrations", dbName)
	if errors.Is(err, migrate.ErrLockTimeout) {
		// The migrations gave up waiting for the migration lock, but
		// their request for it stays pending on the connection until
		// another migrator releases the lock, and closing the
		// connection would wait for it. Hand the connection back in
		// the background instead, once the request is done. The
		// connection runs one query at a time, so the unlock runs
		// after the request and releases the lock it may have got.
		go func() {
			_, _ = conn.ExecContext(
				ctx, "SELECT pg_advisory_unlock_all()",
			)
			_ = conn.Close()
		}()

		return err
	}

	_ = conn.Close()

	return err
}

// NewTestPostgresDB is a helper function that creates a Postgres database for
// testing.
func NewTestPostgresDB(t *testing.T) *PostgresStore {
	t.Helper()

	t.Logf("Creating new Postgres DB for testing")

	sqlFixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	store, err := NewPostgresStore(
		sqlFixture.GetConfig(), &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		sqlFixture.TearDown(t)
	})

	return store
}
