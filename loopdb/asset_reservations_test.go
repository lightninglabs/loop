package loopdb_test

import (
	"bytes"
	"database/sql"
	"testing"
	"time"

	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/stretchr/testify/require"
)

func TestAssetReservationSchema(t *testing.T) {
	db := loopdb.NewTestDB(t)
	ctx := t.Context()
	now := time.Unix(1700000000, 0).UTC()
	args := sqlc.CreateAssetReservationParams{
		ReservationID:         bytes.Repeat([]byte{1}, 32),
		AssetID:               bytes.Repeat([]byte{2}, 32),
		Amount:                10000,
		Fee:                   10,
		CsvDelay:              1440,
		RequiredConfirmations: 3,
		ExecutionDelta:        90,
		MinUsableBlocks:       1000,
		ClientPubkey:          bytes.Repeat([]byte{3}, 33),
		ClientKeyFamily:       99,
		ClientKeyIndex:        1,
		CreatedAt:             now,
	}

	// A failed history write must not leave a partially created purchase.
	err := db.ExecTx(ctx, &loopdb.SqliteTxOptions{}, func(q *sqlc.Queries) error {
		_, err := q.CreateAssetReservation(ctx, args)
		if err != nil {
			return err
		}
		return q.InsertAssetReservationUpdate(ctx,
			sqlc.InsertAssetReservationUpdateParams{
				ReservationID:   args.ReservationID,
				UpdateTimestamp: now,
			},
		)
	})
	require.Error(t, err)
	_, err = db.GetAssetReservation(ctx, args.ReservationID)
	require.ErrorIs(t, err, sql.ErrNoRows)

	count, err := db.CreateAssetReservation(ctx, args)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)

	// Reusing an ID cannot replace the accepted terms.
	changed := args
	changed.Amount = 20000
	changed.Fee = 20
	count, err = db.CreateAssetReservation(ctx, changed)
	require.NoError(t, err)
	require.Zero(t, count)
	stored, err := db.GetAssetReservation(ctx, args.ReservationID)
	require.NoError(t, err)
	require.Equal(t, args.Amount, stored.Amount)
	require.Equal(t, args.Fee, stored.Fee)
	require.Equal(t, args.RequiredConfirmations, stored.RequiredConfirmations)

	// Store quoted fees on either side of the initial server price.
	// A database read must not replace them with the current policy.
	for i, fee := range []int64{9, 11} {
		quoted := args
		quoted.ReservationID = bytes.Repeat([]byte{byte(i + 5)}, 32)
		quoted.Fee = fee
		count, err := db.CreateAssetReservation(ctx, quoted)
		require.NoError(t, err)
		require.EqualValues(t, 1, count)
		row, err := db.GetAssetReservation(ctx, quoted.ReservationID)
		require.NoError(t, err)
		require.Equal(t, fee, row.Fee)
	}

	// History order comes from its sequence, not wall-clock differences.
	for _, state := range []string{"Created", "Waiting"} {
		err := db.InsertAssetReservationUpdate(ctx,
			sqlc.InsertAssetReservationUpdateParams{
				ReservationID:   args.ReservationID,
				UpdateState:     state,
				UpdateTimestamp: now,
			},
		)
		require.NoError(t, err)
	}
	updates, err := db.GetAssetReservationUpdates(ctx, args.ReservationID)
	require.NoError(t, err)
	require.Len(t, updates, 2)
	require.Equal(t, "Created", updates[0].UpdateState)
	require.Equal(t, "Waiting", updates[1].UpdateState)

	tests := []struct {
		name   string
		change func(*sqlc.CreateAssetReservationParams)
	}{
		{
			"short ID",
			func(p *sqlc.CreateAssetReservationParams) {
				p.ReservationID = []byte{1}
			},
		},
		{
			"short asset",
			func(p *sqlc.CreateAssetReservationParams) {
				p.AssetID = []byte{1}
			},
		},
		{
			"short key",
			func(p *sqlc.CreateAssetReservationParams) {
				p.ClientPubkey = []byte{1}
			},
		},
		{
			"zero amount",
			func(p *sqlc.CreateAssetReservationParams) {
				p.Amount = 0
			},
		},
		{
			"zero fee",
			func(p *sqlc.CreateAssetReservationParams) {
				p.Fee = 0
			},
		},
		{
			"negative fee",
			func(p *sqlc.CreateAssetReservationParams) {
				p.Fee = -1
			},
		},
		{
			"fee overflow",
			func(p *sqlc.CreateAssetReservationParams) {
				p.Amount = 9223372036854775807
				p.Fee = 9223372036854776
			},
		},
		{
			"CSV flags",
			func(p *sqlc.CreateAssetReservationParams) {
				p.CsvDelay |= 1 << 22
			},
		},
		{
			"zero depth",
			func(p *sqlc.CreateAssetReservationParams) {
				p.RequiredConfirmations = 0
			},
		},
		{
			"no delivery window",
			func(p *sqlc.CreateAssetReservationParams) {
				p.MinUsableBlocks = 1349
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := args
			invalid.ReservationID = bytes.Repeat([]byte{9}, 32)
			test.change(&invalid)
			_, err := db.CreateAssetReservation(ctx, invalid)
			require.Error(t, err)
		})
	}
}

func TestAssetReservationPendingQuote(t *testing.T) {
	db := loopdb.NewTestDB(t)
	ctx := t.Context()
	id := bytes.Repeat([]byte{7}, 32)
	_, err := db.CreateAssetReservation(ctx,
		sqlc.CreateAssetReservationParams{
			ReservationID: id,
			AssetID:       bytes.Repeat([]byte{2}, 32),
			Amount:        10000,
			ClientPubkey:  bytes.Repeat([]byte{3}, 33),
			CreatedAt:     time.Now().UTC(),
		},
	)
	require.NoError(t, err)
	row, err := db.GetAssetReservation(ctx, id)
	require.NoError(t, err)
	require.Zero(t, row.Fee)
	require.Empty(t, row.Quote)

	// Quote terms arrive as one complete set, never as partial defaults.
	count, err := db.UpdateAssetReservationPurchase(ctx,
		sqlc.UpdateAssetReservationPurchaseParams{
			ReservationID:         id,
			Fee:                   11,
			CsvDelay:              1440,
			RequiredConfirmations: 3,
			ExecutionDelta:        90,
			MinUsableBlocks:       1000,
			Quote:                 []byte{1, 2, 3},
		},
	)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	row, err = db.GetAssetReservation(ctx, id)
	require.NoError(t, err)
	require.EqualValues(t, 11, row.Fee)
	require.EqualValues(t, 3, row.RequiredConfirmations)
	require.Equal(t, []byte{1, 2, 3}, row.Quote)
}
