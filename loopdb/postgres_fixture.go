package loopdb

import (
	"context"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const (
	testPgUser   = "test"
	testPgPass   = "test"
	testPgDBName = "test"
)

// TestPgFixture is a test fixture that starts a Postgres 11 instance using an
// embedded Postgres runtime.
type TestPgFixture struct {
	db          *sql.DB
	pg          *embeddedpostgres.EmbeddedPostgres
	host        string
	port        int
	expiryTimer *time.Timer
	stopOnce    sync.Once
}

// NewTestPgFixture constructs a new TestPgFixture starting up an embedded
// Postgres 11 server. The process will be auto-stopped after the specified
// expiry if TearDown is not called first.
func NewTestPgFixture(t *testing.T, expiry time.Duration) *TestPgFixture {
	port := getFreePort(t)
	runtimePath := t.TempDir()
	logDir := filepath.Join(runtimePath, "logs")
	require.NoError(t, os.MkdirAll(logDir, 0o755))
	config := embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V11).
		Database(testPgDBName).
		Username(testPgUser).
		Password(testPgPass).
		Port(uint32(port)).
		RuntimePath(runtimePath).
		StartParameters(map[string]string{
			"listen_addresses": "127.0.0.1",

			// Logging collector only captures stderr output, so
			// keep this destination in sync with the log file
			// settings below.
			"log_statement":     "all",
			"log_destination":   "stderr",
			"logging_collector": "on",
			"log_directory":     logDir,
			"log_filename":      "postgres.log",
		})

	pg := embeddedpostgres.NewDatabase(config)
	require.NoError(t, pg.Start(), "Could not start embedded Postgres")

	fixture := &TestPgFixture{
		host: "127.0.0.1",
		port: port,
		pg:   pg,
	}

	if expiry > 0 {
		fixture.expiryTimer = time.AfterFunc(expiry, func() {
			log.Warnf("Postgres fixture exceeded lifetime; tearing down")
			fixture.TearDown(t)
		})
	}

	databaseURL := fixture.GetDSN()
	log.Infof("Connecting to Postgres fixture: %v\n", databaseURL)

	testDB, err := sql.Open("postgres", databaseURL)
	require.NoError(t, err, "Could not open connection to Postgres fixture")
	require.NoError(t, testDB.Ping(), "Could not connect to embedded Postgres")
	fixture.db = testDB

	return fixture
}

// GetDSN returns the DSN (Data Source Name) for the started Postgres node.
func (f *TestPgFixture) GetDSN() string {
	return f.GetConfig().DSN(false)
}

// GetConfig returns the full config of the Postgres node.
func (f *TestPgFixture) GetConfig() *PostgresConfig {
	return &PostgresConfig{
		Host:       f.host,
		Port:       f.port,
		User:       testPgUser,
		Password:   testPgPass,
		DBName:     testPgDBName,
		RequireSSL: false,
	}
}

// TearDown stops the embedded Postgres process and releases resources.
func (f *TestPgFixture) TearDown(t *testing.T) {
	if f.expiryTimer != nil {
		f.expiryTimer.Stop()
	}

	f.stopOnce.Do(func() {
		if f.db != nil {
			err := f.db.Close()
			require.NoErrorf(t, err, "failed to close postgres")
		}

		if f.pg != nil {
			err := f.pg.Stop()
			require.NoErrorf(t, err, "failed to stop postgres")
		}

		f.db = nil
		f.pg = nil
	})
}

// ClearDB clears the database.
func (f *TestPgFixture) ClearDB(t *testing.T) {
	dbConn, err := sql.Open("postgres", f.GetDSN())
	require.NoError(t, err)

	_, err = dbConn.ExecContext(
		context.Background(),
		`DROP SCHEMA IF EXISTS public CASCADE;
		 CREATE SCHEMA public;`,
	)
	require.NoError(t, err)
}

// getFreePort returns an available TCP port on localhost.
func getFreePort(t *testing.T) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defer func() {
		require.NoError(t, listener.Close())
	}()

	return listener.Addr().(*net.TCPAddr).Port
}
