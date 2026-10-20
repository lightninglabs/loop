package loopin

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
)

func writeLegacyAssets(
	t *testing.T,
	path string,
	buckets map[string]map[string]any,
) {

	t.Helper()
	db, err := bbolt.Open(path, 0600, nil)
	require.NoError(t, err)
	require.NoError(t, db.Update(func(tx *bbolt.Tx) error {
		for name, rows := range buckets {
			bucket, err := tx.CreateBucketIfNotExists([]byte(name))
			if err != nil {
				return err
			}
			for key, value := range rows {
				raw, err := json.Marshal(value)
				if unencoded, ok := value.([]byte); ok {
					raw = unencoded
				}
				if err != nil {
					return err
				}
				if err := bucket.Put([]byte(key), raw); err != nil {
					return err
				}
			}
		}
		return nil
	}))
	require.NoError(t, db.Close())
}

func TestImportAssetJournalsPreservesExposureAndNativeAmounts(t *testing.T) {
	dir := t.TempDir()
	inventoryPath, executionPath := filepath.Join(
		dir,
		"inventory.db",
	), filepath.Join(
		dir,
		"execution.db",
	)
	store, err := openAssetTestStore(filepath.Join(dir, "loop.db"))
	require.NoError(t, err)
	d := testDeposit(t)
	d.Amount = math.MaxUint64 - 1
	d.ValueSat = 330
	job := testSwap(d, 1)
	r := assetRecord(d, job.ID)
	job.Offer = &r.Offer
	job.Intent = r.InvoiceIntent
	r.Revision = 7
	r.Reserved = true
	r.InvoiceCreating = true
	r.Invoice = assetexec.Invoice{
		PaymentRequest: "original hold invoice",
		PaymentPlan:    []byte("same-group RFQ"),
	}
	r.ServerFallback = []byte("original fallback")
	r.Fallback = assetexec.Fallback{
		Bundle:          []byte("signed recovery"),
		ClaimSignatures: []byte("claim authority"),
	}
	r.AcceptedEvidence = []byte("accepted HTLC evidence")
	r.SignaturesExposed = true
	r.State = assetexec.MonitorInvoiceAndHtlcTx
	r.DepositState = "AssetClaimExposed"
	writeLegacyAssets(t, inventoryPath, map[string]map[string]any{
		"requests-v1": {string(job.ID[:]): job},
		"inventory-v1": {
			d.Outpoint: AssetInventoryEntry{
				Deposit:   d,
				RequestID: job.ID,
				Hash:      r.Offer.Hash,
			},
		},
		"released-swaps-v1": {},
	})
	writeLegacyAssets(t, executionPath, map[string]map[string]any{
		"asset-loopin-client-v1": {string(r.Offer.Hash[:]): r},
	})
	require.NoError(
		t,
		store.ImportLegacyAssetJournals(
			t.Context(),
			inventoryPath,
			executionPath,
		),
	)
	require.NoError(t, store.Close())
	store, err = openAssetTestStore(filepath.Join(dir, "loop.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	restored, err := store.Get(t.Context(), r.Offer.Hash)
	require.NoError(t, err)
	require.Equal(t, r.Invoice, restored.Invoice)
	require.Equal(t, r.Fallback, restored.Fallback)
	require.True(t, restored.SignaturesExposed)
	require.False(t, restored.HasPreimage, "only the server can execute")
	inventory, err := store.AssetInventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, d.Amount, inventory[0].Deposit.Amount)
	require.Equal(t, job.ID, inventory[0].RequestID)
	bitcoinDeposits, err := deposit.NewSqlStore(store.db.BaseDB).
		AllDeposits(t.Context())
	require.NoError(t, err)
	require.Empty(t, bitcoinDeposits)
	_, err = deposit.NewSqlStore(store.db.BaseDB).
		DepositForOutpoint(t.Context(), d.Outpoint)
	require.ErrorIs(t, err, deposit.ErrDepositNotFound)
	// Reopening with the old files present must not overwrite newer SQL state.
	restored.CancellationRequested = true
	restored.State = assetexec.SweepHtlcTimeout
	restored.DepositState = "SweepHtlcTimeout"
	restored.Revision++
	require.NoError(t, store.Update(t.Context(), restored, 7))
	require.NoError(
		t,
		store.ImportLegacyAssetJournals(
			t.Context(),
			inventoryPath,
			executionPath,
		),
	)
	again, err := store.Get(t.Context(), r.Offer.Hash)
	require.NoError(t, err)
	require.True(t, again.CancellationRequested)
	require.EqualValues(t, 8, again.Revision)
}

func TestAssetImportRollsBackIncompleteLegacyPair(t *testing.T) {
	dir := t.TempDir()
	store, err := openAssetTestStore(filepath.Join(dir, "loop.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	r := assetRecord(d, [32]byte{1})
	execution := filepath.Join(dir, "execution.db")
	writeLegacyAssets(
		t,
		execution,
		map[string]map[string]any{
			"asset-loopin-client-v1": {string(r.Offer.Hash[:]): r},
		},
	)
	require.Error(
		t,
		store.ImportLegacyAssetJournals(
			t.Context(),
			filepath.Join(dir, "missing.db"),
			execution,
		),
	)
	jobs, err := store.AssetJobs(t.Context())
	require.NoError(t, err)
	require.Empty(t, jobs)
	_, err = store.db.GetMigration(t.Context(), assetJournalMigration)
	require.Error(t, err)
}

type failAssetReleaseDB struct{ BaseDB }
type failAssetReleaseQueries struct{ Querier }

func (q failAssetReleaseQueries) ReleaseAssetDeposit(
	ctx context.Context,
	p sqlc.ReleaseAssetDepositParams,
) (int64, error) {

	_, err := q.Querier.ReleaseAssetDeposit(ctx, p)
	if err != nil {
		return 0, err
	}
	return 0, errors.New("lost transaction before commit")
}

func (d failAssetReleaseDB) ExecTx(
	ctx context.Context,
	opts loopdb.TxOptions,
	fn func(Querier) error,
) error {

	return d.BaseDB.ExecTx(
		ctx,
		opts,
		func(q Querier) error { return fn(failAssetReleaseQueries{q}) },
	)
}

func TestAssetReleaseRollbackPreservesReservationAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.db")
	store, err := openAssetTestStore(path)
	require.NoError(t, err)
	d := testDeposit(t)
	job := testSwap(d, 1)
	r := assetRecord(d, job.ID)
	job.Offer = &r.Offer
	job.Intent = r.InvoiceIntent
	require.NoError(t, store.AddAssetDeposit(t.Context(), d))
	require.NoError(t, store.PutAssetJob(t.Context(), job))
	require.NoError(t, store.Create(t.Context(), r))
	r.State = assetexec.Failed
	r.CancellationRequested = true
	r.Revision = 1
	store.baseDB = failAssetReleaseDB{store.baseDB}
	r.DepositsReleased = true
	r.Revision = 1
	require.Error(t, store.Update(t.Context(), r, 0))
	require.NoError(t, store.Close())
	store, err = openAssetTestStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	restored, err := store.Get(t.Context(), r.Offer.Hash)
	require.NoError(t, err)
	require.False(t, restored.DepositsReleased)
	require.EqualValues(t, 0, restored.Revision)
	require.Equal(t, assetexec.InitHtlcTx, restored.State)
	entries, err := store.AssetInventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, job.ID, entries[0].RequestID)
	require.NoError(t, store.Update(t.Context(), r, 0))
	require.NoError(t, store.PutAssetJob(t.Context(), testSwap(d, 2)))
	// A stale revision cannot remove the new admission's ownership.
	require.ErrorIs(t, store.Update(t.Context(), r, 0), assetexec.ErrConflict)
}

// TestAssetSharedSQLBackend runs unchanged against SQLite and PostgreSQL with
// the repository's test_db_postgres build tag.
func TestAssetSharedSQLBackend(t *testing.T) {
	db := loopdb.NewTestDB(t)
	store := NewSqlStore(loopdb.NewTypedStore[Querier](db), clock.NewDefaultClock(), &chaincfg.RegressionNetParams)
	d := testDeposit(t)
	require.NoError(t, store.AddAssetDeposit(t.Context(), d))
	bad := d
	bad.Outpoint = "AB" + d.Outpoint[2:]
	require.ErrorIs(t, store.AddAssetDeposit(t.Context(), bad), assetexec.ErrInvalid)
	job := testSwap(d, 1)
	r := assetRecord(d, job.ID)
	job.Offer = &r.Offer
	job.Intent = r.InvoiceIntent
	require.NoError(t, store.PutAssetJob(t.Context(), job))
	require.NoError(t, store.Create(t.Context(), r))
	require.ErrorIs(t, store.PutAssetJob(t.Context(), testSwap(d, 2)), assetexec.ErrConflict)
	r.Reserved = true
	r.DepositState = "LoopingIn"
	r.Revision++
	require.NoError(t, store.Update(t.Context(), r, 0))
	require.ErrorIs(t, store.Update(t.Context(), r, 0), assetexec.ErrConflict)
	btc := deposit.NewSqlStore(db.BaseDB)
	all, err := btc.AllDeposits(t.Context())
	require.NoError(t, err)
	require.Empty(t, all)
	op, err := assetPoint(d.Outpoint)
	require.NoError(t, err)
	row, err := db.GetAssetDeposit(t.Context(), sqlc.GetAssetDepositParams{TxHash: op.Hash[:], OutIndex: int32(op.Index)})
	require.NoError(t, err)
	update, err := db.GetLatestDepositUpdate(t.Context(), row.DepositID)
	require.NoError(t, err)
	require.Equal(t, "LoopingIn", update.UpdateState)
	// Ordinary swap ownership operations cannot claim an asset anchor.
	require.NoError(t, db.MapDepositToSwap(
		t.Context(), sqlc.MapDepositToSwapParams{
			DepositID: row.DepositID, SwapHash: []byte("bitcoin swap"),
		},
	))
	saved, err := store.Get(t.Context(), r.Offer.Hash)
	require.NoError(t, err)
	require.Equal(t, r.Revision, saved.Revision)
	entries, err := store.AssetInventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, r.Offer.Hash, entries[0].Hash)
}

func TestAssetRecoveryRejectsMismatchedSQLRecord(t *testing.T) {
	for _, mutation := range []string{"revision", "request", "offer", "intent", "outpoint"} {
		t.Run(mutation, func(t *testing.T) {
			store, err := openAssetTestStore(filepath.Join(t.TempDir(), "loop.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			d := testDeposit(t)
			job := testSwap(d, 1)
			r := assetRecord(d, job.ID)
			requestID := job.ID
			job.Offer = &r.Offer
			job.Intent = r.InvoiceIntent
			require.NoError(t, store.AddAssetDeposit(t.Context(), d))
			require.NoError(t, store.PutAssetJob(t.Context(), job))
			bad := *r
			bad.InvoiceIntent = []byte("changed intent")
			require.ErrorIs(t, store.Create(t.Context(), &bad), assetexec.ErrConflict)
			require.NoError(t, store.Create(t.Context(), r))
			if mutation == "revision" {
				raw, err := json.Marshal(r)
				require.NoError(t, err)
				_, err = store.db.UpdateAssetRecord(
					t.Context(), sqlc.UpdateAssetRecordParams{
						SwapHash: r.Offer.Hash[:], RecordData: raw, Revision: 1,
					},
				)
				require.NoError(t, err)
			} else {
				switch mutation {
				case "request":
					job.ID = [32]byte{9}
				case "offer":
					job.Offer.Terms.Contract = []byte("different anchor")
				case "intent":
					job.Intent = []byte("different RFQ")
				case "outpoint":
					job.Swap.Outpoint = wire.OutPoint{Index: 2}.String()
				}
				raw, err := json.Marshal(job)
				require.NoError(t, err)
				require.NoError(t, store.db.UpdateAssetJob(
					t.Context(), sqlc.UpdateAssetJobParams{
						RequestID: requestID[:], JobData: raw,
					},
				))
			}
			_, err = store.Get(t.Context(), r.Offer.Hash)
			require.ErrorIs(t, err, assetexec.ErrInvalid)
			_, err = store.ListActive(t.Context())
			require.ErrorIs(t, err, assetexec.ErrInvalid)
		})
	}
}

func TestAssetImportReleaseReceiptKeepsNewReservation(t *testing.T) {
	dir := t.TempDir()
	store, err := openAssetTestStore(filepath.Join(dir, "loop.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	old, next := testSwap(d, 1), testSwap(d, 2)
	r := assetRecord(d, old.ID)
	r.State = assetexec.Failed
	r.CancellationRequested = true
	r.Revision = 1
	old.Offer = &r.Offer
	old.Intent = r.InvoiceIntent
	receipt := append(append([]byte{}, old.ID[:]...), []byte(d.Outpoint)...)
	inventory, execution := filepath.Join(dir, "inventory.db"), filepath.Join(dir, "execution.db")
	writeLegacyAssets(t, inventory, map[string]map[string]any{
		"requests-v1":       {string(old.ID[:]): old, string(next.ID[:]): next},
		"inventory-v1":      {d.Outpoint: AssetInventoryEntry{Deposit: d, RequestID: next.ID}},
		"released-swaps-v1": {string(r.Offer.Hash[:]): receipt},
	})
	writeLegacyAssets(t, execution, map[string]map[string]any{"asset-loopin-client-v1": {string(r.Offer.Hash[:]): r}})
	require.NoError(t, store.ImportLegacyAssetJournals(t.Context(), inventory, execution))
	saved, err := store.Get(t.Context(), r.Offer.Hash)
	require.NoError(t, err)
	require.True(t, saved.DepositsReleased)
	entries, err := store.AssetInventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, next.ID, entries[0].RequestID)
	active, err := store.ListActive(t.Context())
	require.NoError(t, err)
	require.Empty(t, active)
}

func TestAssetImportCompletesInterruptedBinding(t *testing.T) {
	dir := t.TempDir()
	store, err := openAssetTestStore(filepath.Join(dir, "loop.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	job := testSwap(d, 1)
	r := assetRecord(d, job.ID)
	job.Offer = &r.Offer
	job.Intent = r.InvoiceIntent
	inventory := filepath.Join(dir, "inventory.db")
	execution := filepath.Join(dir, "execution.db")
	writeLegacyAssets(t, inventory, map[string]map[string]any{
		"requests-v1": {string(job.ID[:]): job},
		"inventory-v1": {d.Outpoint: AssetInventoryEntry{
			Deposit: d, RequestID: job.ID,
		}},
		"released-swaps-v1": {},
	})
	writeLegacyAssets(t, execution, map[string]map[string]any{
		"asset-loopin-client-v1": {string(r.Offer.Hash[:]): r},
	})
	require.NoError(t, store.ImportLegacyAssetJournals(
		t.Context(), inventory, execution,
	))
	entries, err := store.AssetInventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, r.Offer.Hash, entries[0].Hash)
	r.CancellationRequested = true
	r.State = assetexec.Failed
	r.DepositsReleased = true
	r.Revision = 1
	require.NoError(t, store.Update(t.Context(), r, 0))
	require.NoError(t, store.PutAssetJob(t.Context(), testSwap(d, 2)))
}
