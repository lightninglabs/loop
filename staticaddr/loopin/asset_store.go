package loopin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"math"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit"
)

// assetTx serializes local admissions and combines inventory and recovery
// changes in the same normal Loop database transaction.
func (s *SqlStore) assetTx(ctx context.Context, fn func(Querier) error) error {
	s.assetMu.Lock()
	defer s.assetMu.Unlock()
	err := s.baseDB.ExecTx(ctx, loopdb.NewSqlWriteOpts(), fn)
	var conflict *loopdb.ErrSqlUniqueConstraintViolation
	if errors.As(loopdb.MapSQLError(err), &conflict) {
		return assetexec.ErrConflict
	}
	if errors.Is(err, sql.ErrNoRows) {
		return assetexec.ErrNotFound
	}
	return err
}

func assetPoint(point string) (*wire.OutPoint, error) {
	op, err := wire.NewOutPointFromString(point)
	if err != nil || op.String() != point {
		return nil, assetexec.ErrInvalid
	}
	return op, nil
}

func assetDeposit(
	ctx context.Context,
	q Querier,
	point string,
) (sqlc.Deposit, AssetInventoryEntry, error) {

	op, err := assetPoint(point)
	if err != nil {
		return sqlc.Deposit{}, AssetInventoryEntry{}, err
	}
	row, err := q.GetAssetDeposit(
		ctx,
		sqlc.GetAssetDepositParams{
			TxHash:   op.Hash[:],
			OutIndex: int32(op.Index),
		},
	)
	if err != nil {
		return row, AssetInventoryEntry{}, err
	}
	entry, err := assetInventoryEntry(row)
	return row, entry, err
}

func assetInventoryEntry(row sqlc.Deposit) (AssetInventoryEntry, error) {
	var e AssetInventoryEntry
	if len(row.AssetData) == 0 {
		return e, assetexec.ErrConflict
	}
	if err := json.Unmarshal(row.AssetData, &e.Deposit); err != nil {
		return e, err
	}
	if (len(row.AssetRequestID) != 0 && len(row.AssetRequestID) != 32) ||
		(len(row.SwapHash) != 0 && len(row.SwapHash) != 32) {

		return e, assetexec.ErrInvalid
	}
	copy(e.RequestID[:], row.AssetRequestID)
	copy(e.Hash[:], row.SwapHash)
	e.Consumed = row.AssetConsumed
	return e, nil
}

func assetChanged(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows != 1 {
		return assetexec.ErrConflict
	}
	return nil
}

// PutAssetJob persists immutable request identity and reserves its deposit
// before funding, quote creation, or any other external effect.
func (s *SqlStore) PutAssetJob(ctx context.Context, j *AssetJob) error {
	if j == nil || j.ID == ([32]byte{}) {
		return assetexec.ErrInvalid
	}
	requests := 0
	for _, present := range []bool{
		j.Deposit != nil, j.Swap != nil, j.Refund != nil,
	} {
		if present {
			requests++
		}
	}
	if requests != 1 || (j.Deposit != nil && j.Deposit.RequestID != j.ID) ||
		(j.Swap != nil && j.Swap.RequestID != j.ID) {

		return assetexec.ErrInvalid
	}
	return s.assetTx(ctx, func(q Querier) error {
		old, err := q.GetAssetRequest(ctx, j.ID[:])
		if err == nil {
			var prior AssetJob
			if err := json.Unmarshal(old.JobData, &prior); err != nil {
				return err
			}
			a, _ := json.Marshal([]any{prior.Deposit, prior.Swap, prior.Refund})
			b, _ := json.Marshal([]any{j.Deposit, j.Swap, j.Refund})
			if !bytes.Equal(a, b) {
				return assetexec.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if j.Swap != nil || j.Refund != nil {
			var point string
			if j.Swap != nil {
				point = j.Swap.Outpoint
			} else {
				point = j.Refund.Outpoint
			}
			row, entry, err := assetDeposit(ctx, q, point)
			if err != nil {
				return err
			}
			if j.Swap != nil &&
				(entry.Deposit.GroupKey != j.Swap.GroupKey ||
					!validAssetSwapAmount(entry.Deposit.Amount, *j.Swap)) {

				return assetexec.ErrConflict
			}
			reserveAssetDepositArgs := sqlc.ReserveAssetDepositParams{
				DepositID:      row.DepositID,
				AssetRequestID: j.ID[:],
			}
			rows, err := q.ReserveAssetDeposit(ctx, reserveAssetDepositArgs)
			if err := assetChanged(rows, err); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(j)
		if err != nil {
			return err
		}
		return q.InsertAssetRequest(
			ctx,
			sqlc.InsertAssetRequestParams{RequestID: j.ID[:], JobData: raw},
		)
	})
}

func updateAssetJob(ctx context.Context, q Querier, j *AssetJob) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return q.UpdateAssetJob(
		ctx,
		sqlc.UpdateAssetJobParams{RequestID: j.ID[:], JobData: raw},
	)
}

// UpdateAssetJob records recovered funding, offer, and notification results.
func (s *SqlStore) UpdateAssetJob(ctx context.Context, j *AssetJob) error {
	return s.assetTx(ctx, func(q Querier) error {
		if _, err := q.GetAssetRequest(ctx, j.ID[:]); err != nil {
			return err
		}
		return updateAssetJob(ctx, q, j)
	})
}

// GetAssetJob returns the immutable request and current recovery outbox.
func (s *SqlStore) GetAssetJob(
	ctx context.Context,
	id [32]byte,
) (*AssetJob, error) {

	row, err := s.baseDB.GetAssetRequest(ctx, id[:])
	if errors.Is(err, sql.ErrNoRows) {
		return nil, assetexec.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var j AssetJob
	if err := json.Unmarshal(row.JobData, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// AssetJobs returns independent request snapshots for status and recovery.
func (s *SqlStore) AssetJobs(ctx context.Context) ([]AssetJob, error) {
	rows, err := s.baseDB.ListAssetRequests(ctx)
	if err != nil {
		return nil, err
	}
	jobs := make([]AssetJob, 0, len(rows))
	for _, row := range rows {
		var j AssetJob
		if err := json.Unmarshal(row.JobData, &j); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// AssetInventory returns asset metadata from the normal deposit inventory.
func (s *SqlStore) AssetInventory(
	ctx context.Context,
) ([]AssetInventoryEntry, error) {

	rows, err := s.baseDB.ListAssetDeposits(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]AssetInventoryEntry, 0, len(rows))
	for _, row := range rows {
		e, err := assetInventoryEntry(row)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// AddAssetDeposit persists a confirmed asset anchor in the normal inventory.
func (s *SqlStore) AddAssetDeposit(
	ctx context.Context,
	d swapkit.Deposit,
) error {

	return s.assetTx(
		ctx,
		func(q Querier) error { return s.addAssetDeposit(ctx, q, d) },
	)
}

func (s *SqlStore) addAssetDeposit(
	ctx context.Context,
	q Querier,
	d swapkit.Deposit,
) error {

	op, err := assetPoint(d.Outpoint)
	if err != nil || d.ValueSat > math.MaxInt64 {
		return assetexec.ErrInvalid
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	row, err := q.GetAssetDeposit(
		ctx,
		sqlc.GetAssetDepositParams{
			TxHash:   op.Hash[:],
			OutIndex: int32(op.Index),
		},
	)
	if err == nil {
		entry, err := assetInventoryEntry(row)
		if err != nil {
			return err
		}
		if entry.Deposit.GroupKey != d.GroupKey ||
			entry.Deposit.Amount != d.Amount ||
			!bytes.Equal(entry.Deposit.Contract, d.Contract) {

			return assetexec.ErrConflict
		}
		return q.UpdateAssetDeposit(
			ctx,
			sqlc.UpdateAssetDepositParams{
				DepositID:          row.DepositID,
				AssetData:          raw,
				ConfirmationHeight: int64(d.ConfirmationHeight),
			},
		)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	id := sha256.Sum256([]byte("asset-deposit:" + d.Outpoint))
	err = q.InsertAssetDeposit(
		ctx,
		sqlc.InsertAssetDepositParams{
			DepositID:            id[:],
			TxHash:               op.Hash[:],
			OutIndex:             int32(op.Index),
			Amount:               int64(d.ValueSat),
			ConfirmationHeight:   int64(d.ConfirmationHeight),
			TimeoutSweepPkScript: []byte{},
			AssetData:            raw,
		},
	)
	if err != nil {
		return err
	}
	return s.assetDepositState(ctx, q, id[:], "Deposited")
}

func (s *SqlStore) assetDepositState(
	ctx context.Context,
	q Querier,
	id []byte,
	state string,
) error {

	return q.InsertDepositUpdate(
		ctx,
		sqlc.InsertDepositUpdateParams{
			DepositID:       id,
			UpdateState:     state,
			UpdateTimestamp: sqlStoreUpdateTime(s.clock),
		},
	)
}

// FinishAssetRefund commits the original-anchor refund proof and consumption.
func (s *SqlStore) FinishAssetRefund(
	ctx context.Context,
	j *AssetJob,
	proof []byte,
) error {

	if j.Refund == nil || len(proof) == 0 {
		return assetexec.ErrInvalid
	}
	return s.assetTx(ctx, func(q Querier) error {
		row, e, err := assetDeposit(ctx, q, j.Refund.Outpoint)
		if err != nil {
			return err
		}
		if e.Hash != ([32]byte{}) {
			return assetexec.ErrConflict
		}
		releaseAssetDepositArgs := sqlc.ReleaseAssetDepositParams{
			DepositID:      row.DepositID,
			AssetRequestID: j.ID[:],
			AssetConsumed:  true,
		}
		rows, err := q.ReleaseAssetDeposit(ctx, releaseAssetDepositArgs)
		if err := assetChanged(rows, err); err != nil {
			return err
		}
		j.RefundProof = bytes.Clone(proof)
		j.Complete = true
		if err := s.assetDepositState(ctx, q, row.DepositID, "Expired"); err != nil {
			return err
		}
		return updateAssetJob(ctx, q, j)
	})
}

// CancelUnstartedAssetJob releases only requests without an execution record.
func (s *SqlStore) CancelUnstartedAssetJob(
	ctx context.Context,
	j *AssetJob,
) error {

	if j.Swap == nil {
		return assetexec.ErrInvalid
	}
	return s.assetTx(ctx, func(q Querier) error {
		request, err := q.GetAssetRequest(ctx, j.ID[:])
		if err != nil {
			return err
		}
		if len(request.RecordData) != 0 {
			return assetexec.ErrConflict
		}
		row, e, err := assetDeposit(ctx, q, j.Swap.Outpoint)
		if err != nil {
			return err
		}
		if e.Hash != ([32]byte{}) || e.Consumed {
			return assetexec.ErrConflict
		}
		releaseAssetDepositArgs := sqlc.ReleaseAssetDepositParams{
			DepositID:      row.DepositID,
			AssetRequestID: j.ID[:],
		}
		rows, err := q.ReleaseAssetDeposit(ctx, releaseAssetDepositArgs)
		if err := assetChanged(rows, err); err != nil {
			return err
		}
		j.Canceled = true
		return updateAssetJob(ctx, q, j)
	})
}

// Create attaches execution state to its existing durable admission request.
func (s *SqlStore) Create(ctx context.Context, r *assetexec.Record) error {
	if err := assetexec.ValidateCreate(r); err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.assetTx(ctx, func(q Querier) error {
		request, err := q.GetAssetRequest(ctx, r.Offer.Terms.RequestID[:])
		if err != nil {
			return err
		}
		var job AssetJob
		if err := json.Unmarshal(request.JobData, &job); err != nil {
			return err
		}
		if !assetJobMatchesRecord(&job, r) {
			return assetexec.ErrConflict
		}
		for _, point := range r.Offer.Terms.DepositOutpoints {
			row, _, err := assetDeposit(ctx, q, point)
			if err != nil {
				return err
			}
			bindAssetDepositArgs := sqlc.BindAssetDepositParams{
				DepositID:      row.DepositID,
				AssetRequestID: r.Offer.Terms.RequestID[:],
				SwapHash:       r.Offer.Hash[:],
			}
			rows, err := q.BindAssetDeposit(ctx, bindAssetDepositArgs)
			if err := assetChanged(rows, err); err != nil {
				return err
			}
		}
		return assetChanged(
			q.InsertAssetRecord(
				ctx,
				sqlc.InsertAssetRecordParams{
					RequestID:  r.Offer.Terms.RequestID[:],
					SwapHash:   r.Offer.Hash[:],
					RecordData: raw,
				},
			),
		)
	})
}

// Get returns the independently decoded execution snapshot.
func (s *SqlStore) Get(
	ctx context.Context,
	hash [32]byte,
) (*assetexec.Record, error) {

	row, err := s.baseDB.GetAssetRecord(ctx, hash[:])
	if errors.Is(err, sql.ErrNoRows) {
		return nil, assetexec.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r, err := decodeAssetRecord(row)
	if err == nil && (r.Offer.Hash != hash || r.Revision != row.Revision) {
		return nil, assetexec.ErrInvalid
	}
	return r, err
}

// Update atomically advances recovery state and the normal deposit history.
func (s *SqlStore) Update(
	ctx context.Context,
	r *assetexec.Record,
	previous int64,
) error {

	if previous < 0 || previous == math.MaxInt64 || r.Revision != previous+1 {
		return assetexec.ErrInvalid
	}
	if err := assetexec.ValidateRecord(r); err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.assetTx(ctx, func(q Querier) error {
		row, err := q.GetAssetRecord(ctx, r.Offer.Hash[:])
		if err != nil {
			return err
		}
		old, err := decodeAssetRecord(row)
		if err != nil {
			return err
		}
		if old.Revision != previous {
			return assetexec.ErrConflict
		}
		if err := assetexec.ValidateUpdate(old, r); err != nil {
			return err
		}
		updateAssetRecordArgs := sqlc.UpdateAssetRecordParams{
			SwapHash:   r.Offer.Hash[:],
			RecordData: raw,
			Revision:   r.Revision,
			Revision_2: previous,
		}
		rows, err := q.UpdateAssetRecord(ctx, updateAssetRecordArgs)
		if err := assetChanged(rows, err); err != nil {
			return err
		}
		for _, point := range r.Offer.Terms.DepositOutpoints {
			d, _, err := assetDeposit(ctx, q, point)
			if err != nil {
				return err
			}
			if r.DepositsReleased && !old.DepositsReleased {
				releaseAssetDepositArgs := sqlc.ReleaseAssetDepositParams{
					DepositID:      d.DepositID,
					AssetRequestID: r.Offer.Terms.RequestID[:],
					AssetConsumed:  r.State != assetexec.Failed,
					SwapHash:       r.Offer.Hash[:],
				}
				rows, err := q.ReleaseAssetDeposit(ctx, releaseAssetDepositArgs)
				if err := assetChanged(rows, err); err != nil {
					return err
				}
			}
			if r.DepositState != old.DepositState {
				err := s.assetDepositState(
					ctx, q, d.DepositID, string(r.DepositState),
				)
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ListActive includes terminal swaps whose atomic deposit release is pending.
func (s *SqlStore) ListActive(
	ctx context.Context,
) ([]*assetexec.Record, error) {

	rows, err := s.baseDB.ListAssetRequests(ctx)
	if err != nil {
		return nil, err
	}
	var records []*assetexec.Record
	for _, row := range rows {
		if len(row.RecordData) == 0 {
			continue
		}
		r, err := decodeAssetRecord(row)
		if err != nil {
			return nil, err
		}
		if !r.State.Terminal() || !r.DepositsReleased {
			records = append(records, r)
		}
	}
	return records, nil
}

// decodeAssetRecord checks both the immutable request and SQL revision index.
func decodeAssetRecord(
	row sqlc.StaticAddressAssetRequest,
) (*assetexec.Record, error) {

	r, err := assetexec.DecodeRecord(row.RecordData)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(row.SwapHash, r.Offer.Hash[:]) ||
		!bytes.Equal(row.RequestID, r.Offer.Terms.RequestID[:]) ||
		row.Revision != r.Revision {

		return nil, assetexec.ErrInvalid
	}
	var job AssetJob
	if err := json.Unmarshal(row.JobData, &job); err != nil {
		return nil, err
	}
	if !assetJobMatchesRecord(&job, r) {
		return nil, assetexec.ErrInvalid
	}
	return r, nil
}

func assetJobMatchesRecord(job *AssetJob, r *assetexec.Record) bool {
	a, err := json.Marshal(job.Offer)
	if err != nil {
		return false
	}
	b, err := json.Marshal(r.Offer)
	return err == nil && job.ID == r.Offer.Terms.RequestID &&
		job.Swap != nil && job.Swap.RequestID == job.ID &&
		len(r.Offer.Terms.DepositOutpoints) == 1 &&
		job.Swap.Outpoint == r.Offer.Terms.DepositOutpoints[0] &&
		job.Swap.GroupKey == r.Offer.Terms.GroupKey &&
		job.Swap.Amount == r.Offer.Terms.PayoutAmount &&
		job.Swap.MaxDebit == r.Offer.Terms.MaxDebit &&
		job.Swap.MaxFee >= r.Offer.Terms.AssetFee &&
		bytes.Equal(a, b) && bytes.Equal(job.Intent, r.InvoiceIntent)
}
