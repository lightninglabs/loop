package loopin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"go.etcd.io/bbolt"
)

const assetJournalMigration = "static-asset-journals-v1"

// ImportLegacyAssetJournals atomically imports the draft client's two Bolt
// journals into the normal database. The originals remain untouched for
// backup. A committed migration marker prevents stale copies being replayed.
func (s *SqlStore) ImportLegacyAssetJournals(ctx context.Context,
	inventoryPath, executionPath string) error {

	_, err := s.baseDB.GetMigration(ctx, assetJournalMigration)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	inventory, err := readAssetBuckets(inventoryPath)
	if err != nil {
		return err
	}
	execution, err := readAssetBuckets(executionPath)
	if err != nil {
		return err
	}
	if inventory == nil && execution == nil {
		return nil
	}
	if inventory != nil && (inventory["requests-v1"] == nil ||
		inventory["inventory-v1"] == nil || inventory["released-swaps-v1"] == nil) {

		return assetexec.ErrInvalid
	}
	if execution != nil && execution["asset-loopin-client-v1"] == nil {
		return assetexec.ErrInvalid
	}
	return s.assetTx(ctx, func(q Querier) error {
		if _, err := q.GetMigration(ctx, assetJournalMigration); err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for id, raw := range inventory["requests-v1"] {
			var job AssetJob
			if err := json.Unmarshal(raw, &job); err != nil {
				return err
			}
			if job.ID == ([32]byte{}) || !bytes.Equal([]byte(id), job.ID[:]) {
				return assetexec.ErrInvalid
			}
			insertAssetRequestArgs := sqlc.InsertAssetRequestParams{
				RequestID: job.ID[:],
				JobData:   raw,
			}
			if err := q.InsertAssetRequest(ctx, insertAssetRequestArgs); err != nil {
				return err
			}
		}
		for _, raw := range inventory["inventory-v1"] {
			var entry AssetInventoryEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				return err
			}
			if err := s.addAssetDeposit(ctx, q, entry.Deposit); err != nil {
				return err
			}
			row, _, err := assetDeposit(ctx, q, entry.Deposit.Outpoint)
			if err != nil {
				return err
			}
			var request, hash []byte
			if entry.RequestID != ([32]byte{}) {
				request = entry.RequestID[:]
				if _, err := q.GetAssetRequest(ctx, request); err != nil {
					return err
				}
			}
			if entry.Hash != ([32]byte{}) {
				hash = entry.Hash[:]
			}
			restoreAssetDepositArgs := sqlc.RestoreAssetDepositParams{
				DepositID:      row.DepositID,
				AssetRequestID: request,
				SwapHash:       hash,
				AssetConsumed:  entry.Consumed,
			}
			if err := q.RestoreAssetDeposit(ctx, restoreAssetDepositArgs); err != nil {
				return err
			}
		}
		for hash, raw := range execution["asset-loopin-client-v1"] {
			record, err := assetexec.DecodeRecord(raw)
			if err != nil {
				return err
			}
			if !bytes.Equal([]byte(hash), record.Offer.Hash[:]) {
				return assetexec.ErrInvalid
			}
			// A receipt proves that the old inventory already released this
			// terminal swap before a crash interrupted its journal update.
			receipt := inventory["released-swaps-v1"][hash]
			if len(receipt) > 0 {
				if len(record.Offer.Terms.DepositOutpoints) != 1 ||
					!record.State.Terminal() {

					return assetexec.ErrInvalid
				}
				expected := append(
					bytes.Clone(record.Offer.Terms.RequestID[:]),
					[]byte(record.Offer.Terms.DepositOutpoints[0])...)
				if !bytes.Equal(receipt, expected) {
					return assetexec.ErrConflict
				}
				record.DepositsReleased = true
				raw, err = json.Marshal(record)
				if err != nil {
					return err
				}
			}
			request, err := q.GetAssetRequest(
				ctx,
				record.Offer.Terms.RequestID[:],
			)
			if err != nil {
				return err
			}
			var job AssetJob
			if err := json.Unmarshal(request.JobData, &job); err != nil {
				return err
			}
			if !assetJobMatchesRecord(&job, record) {
				return assetexec.ErrInvalid
			}
			row, entry, err := assetDeposit(
				ctx, q, job.Swap.Outpoint,
			)
			if err != nil {
				return err
			}
			if !record.DepositsReleased {
				if entry.RequestID != job.ID ||
					(entry.Hash != ([32]byte{}) &&
						entry.Hash != record.Offer.Hash) {

					return assetexec.ErrConflict
				}
				// Old Create and inventory binding used separate journals.
				// Repair a crash between them inside this import transaction.
				rows, err := q.BindAssetDeposit(
					ctx, sqlc.BindAssetDepositParams{
						DepositID:      row.DepositID,
						AssetRequestID: job.ID[:],
						SwapHash:       record.Offer.Hash[:],
					},
				)
				if err := assetChanged(rows, err); err != nil {
					return err
				}
				err = s.assetDepositState(
					ctx, q, row.DepositID, string(record.DepositState),
				)
				if err != nil {
					return err
				}
			}
			insertAssetRecordArgs := sqlc.InsertAssetRecordParams{
				RequestID:  job.ID[:],
				SwapHash:   record.Offer.Hash[:],
				RecordData: raw,
				Revision:   record.Revision,
			}
			rows, err := q.InsertAssetRecord(ctx, insertAssetRecordArgs)
			if err := assetChanged(rows, err); err != nil {
				return err
			}
		}
		return q.InsertMigration(
			ctx,
			sqlc.InsertMigrationParams{
				MigrationID: assetJournalMigration,
				MigrationTs: sql.NullTime{
					Time:  s.clock.Now().UTC(),
					Valid: true,
				},
			},
		)
	})
}

// readAssetBuckets reads known legacy buckets under the exclusive file lock.
// Missing files are absent; unreadable or malformed files stop startup.
func readAssetBuckets(path string) (map[string]map[string][]byte, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	db, err := bbolt.Open(
		path,
		0600,
		&bbolt.Options{ReadOnly: true, Timeout: time.Second},
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	buckets := make(map[string]map[string][]byte)
	err = db.View(func(tx *bbolt.Tx) error {
		for _, name := range []string{
			"requests-v1", "inventory-v1", "released-swaps-v1",
			"asset-loopin-client-v1",
		} {
			bucket := tx.Bucket([]byte(name))
			if bucket == nil {
				continue
			}
			values := make(map[string][]byte)
			if err := bucket.ForEach(func(k, v []byte) error {
				if v == nil {
					return assetexec.ErrInvalid
				}
				values[string(k)] = bytes.Clone(v)
				return nil
			}); err != nil {
				return err
			}
			buckets[name] = values
		}
		return nil
	})
	return buckets, err
}
