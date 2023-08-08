package loopdb

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	postgres_migrate "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/stretchr/testify/require"
)

const (
	dsnTemplate = "postgres://%v:%v@%v:%d/%v?sslmode=%v"
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
	Password           string `long:"password" description:"Database user's password."`
	DBName             string `long:"dbname" description:"Database name to use."`
	MaxOpenConnections int32  `long:"maxconnections" description:"Max open connections to keep alive to the database server."`
	RequireSSL         bool   `long:"requiressl" description:"Whether to require using SSL (mode: require) when connecting to the server."`
}

// DSN returns the dns to connect to the database.
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
	cfg *PostgresConfig

	*BaseDB
}

// NewPostgresStore creates a new store that is backed by a Postgres database
// backend.
func NewPostgresStore(cfg *PostgresConfig,
	network *chaincfg.Params) (*PostgresStore, error) {

	log.Infof("Using SQL database '%s'", cfg.DSN(true))

	rawDb, err := sql.Open("pgx", cfg.DSN(false))
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
		driver, err := postgres_migrate.WithInstance(
			rawDb, &postgres_migrate.Config{},
		)
		if err != nil {
			return nil, err
		}

		postgresFS := newReplacerFS(sqlSchemas, map[string]string{
			"BLOB":                "BYTEA",
			"INTEGER PRIMARY KEY": "SERIAL PRIMARY KEY",
		})

		err = applyMigrations(
			postgresFS, driver, "sqlc/migrations", cfg.DBName,
		)
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
