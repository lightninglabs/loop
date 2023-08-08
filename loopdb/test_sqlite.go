//go:build !test_db_postgres
// +build !test_db_postgres

package loopdb

import (
	"testing"
)

var (
	testDBType = "sqlite"
)

// NewTestDB is a helper function that creates an SQLite database for testing.
func NewTestDB(t *testing.T) *SqliteSwapStore {
	return NewTestSqliteDB(t)
}
