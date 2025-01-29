//go:build !test_db_postgres
// +build !test_db_postgres

package loopdb

import (
	"testing"
)

var (
	testDBType = "postgres"
)

// NewTestDB is a helper function that creates a Postgres database for testing.
func NewTestDB(t *testing.T) *PostgresStore {
	return NewTestPostgresDB(t)
}
