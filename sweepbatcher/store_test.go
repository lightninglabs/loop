package sweepbatcher

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/stretchr/testify/require"
)

// TestFetchUnconfirmedSweepBatchesRejectsInvalidBatchTxID verifies that
// malformed persisted batch txids are rejected during batch loading.
func TestFetchUnconfirmedSweepBatchesRejectsInvalidBatchTxID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		txid   string
		errMsg string
	}{
		{
			name:   "short",
			txid:   "abcd",
			errMsg: "invalid batch txid",
		},
		{
			name:   "non-hex",
			txid:   strings.Repeat("z", 64),
			errMsg: "invalid batch txid",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctxb := t.Context()
			testDb := loopdb.NewTestDB(t)
			defer testDb.Close()

			store := NewSQLStore(
				loopdb.NewTypedStore[Querier](testDb),
				&chaincfg.RegressionNetParams,
			)

			_, err := testDb.Queries.InsertBatch(
				ctxb, sqlc.InsertBatchParams{
					BatchTxID: sql.NullString{
						String: test.txid,
						Valid:  true,
					},
					BatchPkScript: []byte{0x00},
					LastRbfHeight: sql.NullInt32{
						Int32: 1,
						Valid: true,
					},
					LastRbfSatPerKw: sql.NullInt32{
						Int32: 1000,
						Valid: true,
					},
					MaxTimeoutDistance: 1,
				},
			)
			require.NoError(t, err)

			_, err = store.FetchUnconfirmedSweepBatches(ctxb)
			require.ErrorContains(t, err, test.errMsg)
		})
	}
}

// TestFetchUnconfirmedSweepBatchesAllowsEmptyBatchTxID verifies that empty
// persisted batch txids are tolerated for recovery robustness.
func TestFetchUnconfirmedSweepBatchesAllowsEmptyBatchTxID(t *testing.T) {
	t.Parallel()

	ctxb := t.Context()
	testDb := loopdb.NewTestDB(t)
	defer testDb.Close()

	store := NewSQLStore(
		loopdb.NewTypedStore[Querier](testDb),
		&chaincfg.RegressionNetParams,
	)

	_, err := testDb.Queries.InsertBatch(
		ctxb, sqlc.InsertBatchParams{
			BatchTxID: sql.NullString{
				String: "",
				Valid:  true,
			},
			BatchPkScript: []byte{0x00},
			LastRbfHeight: sql.NullInt32{
				Int32: 1,
				Valid: true,
			},
			LastRbfSatPerKw: sql.NullInt32{
				Int32: 1000,
				Valid: true,
			},
			MaxTimeoutDistance: 1,
		},
	)
	require.NoError(t, err)

	_, err = store.FetchUnconfirmedSweepBatches(ctxb)
	require.NoError(t, err)
}

// parentBatchDB is a test double that overrides GetParentBatch while reusing
// the rest of the SQL store interface from an embedded BaseDB.
type parentBatchDB struct {
	BaseDB

	batch sqlc.SweepBatch
	err   error
}

// GetParentBatch returns the preconfigured batch row for store tests.
func (s parentBatchDB) GetParentBatch(ctx context.Context,
	outpoint string) (sqlc.SweepBatch, error) {

	return s.batch, s.err
}

// TestGetParentBatchRejectsInvalidBatchTxID verifies that malformed persisted
// batch txids are rejected through the GetParentBatch path as well.
func TestGetParentBatchRejectsInvalidBatchTxID(t *testing.T) {
	t.Parallel()

	store := NewSQLStore(parentBatchDB{
		batch: sqlc.SweepBatch{
			BatchTxID: sql.NullString{
				String: "abcd",
				Valid:  true,
			},
		},
	}, &chaincfg.RegressionNetParams)

	_, err := store.GetParentBatch(t.Context(), wire.OutPoint{})
	require.ErrorContains(t, err, "invalid batch txid")
}
