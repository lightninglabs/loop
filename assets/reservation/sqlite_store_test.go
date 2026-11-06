package reservation

import (
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/stretchr/testify/require"
)

func TestSqliteStoreReopen(t *testing.T) {
	cfg := &loopdb.SqliteConfig{
		DatabaseFileName: filepath.Join(t.TempDir(), "reservations.db"),
	}
	db, err := loopdb.NewSqliteStore(cfg, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	r := testReservation()
	err = NewSqlStore(db.BaseDB).CreateReservation(t.Context(), r)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = loopdb.NewSqliteStore(cfg, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	loaded, err := NewSqlStore(db.BaseDB).GetReservation(t.Context(), r.ID)
	require.NoError(t, err)
	require.Equal(t, r, loaded)
}
