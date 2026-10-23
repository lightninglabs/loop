//go:build test_db_postgres
// +build test_db_postgres

package loopdb

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/require"
)

// TestPostgresConnectionPool tests that a Postgres store bounds its connection
// pool and doesn't keep a connection checked out once it is open.
func TestPostgresConnectionPool(t *testing.T) {
	fixture := NewTestPgFixture(t, DefaultPostgresFixtureLifetime)
	t.Cleanup(func() {
		fixture.TearDown(t)
	})

	testCases := []struct {
		name           string
		maxConnections int32
		wantMaxOpen    int
	}{
		{
			// Without a configured limit, the store uses the same
			// default limit as lnd and tapd.
			name:        "default limit",
			wantMaxOpen: 25,
		},
		{
			name:           "configured limit",
			maxConnections: 3,
			wantMaxOpen:    3,
		},
		{
			// A single connection is enough to open the store,
			// since the migrations hand their connection back.
			name:           "single connection",
			maxConnections: 1,
			wantMaxOpen:    1,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := fixture.GetConfig()
			cfg.MaxOpenConnections = testCase.maxConnections

			store, err := NewPostgresStore(
				cfg, &chaincfg.MainNetParams,
			)
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, store.DB.Close())
			})

			stats := store.DB.Stats()
			require.Equal(
				t, testCase.wantMaxOpen,
				stats.MaxOpenConnections,
			)

			// Opening the store, including running its migrations,
			// must not leave a connection checked out.
			require.Zero(t, stats.InUse)
		})
	}
}
