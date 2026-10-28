package withdraw

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	fsmv2 "github.com/lightninglabs/loop/fsm/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
)

const workflowReservedDepositState = "WithdrawalReserved"

// WorkflowQuerier contains withdrawal-specific durable workflow queries. The
// legacy tables remain available solely as migration evidence.
type WorkflowQuerier interface {
	GetActiveWithdrawalWorkflowMembership(context.Context, []byte) ([]string, error)

	CountWithdrawalDepositUpdatesAtTime(context.Context, sqlc.CountWithdrawalDepositUpdatesAtTimeParams) (int64, error)

	AllStaticAddresses(context.Context) ([]sqlc.StaticAddress, error)

	CreateWithdrawalWorkflow(context.Context, sqlc.CreateWithdrawalWorkflowParams) (int64, error)
	GetWithdrawalWorkflow(context.Context, string) (sqlc.StaticAddressWithdrawalsV2, error)
	GetWithdrawalWorkflowByLegacySource(context.Context, sql.NullString) (sqlc.StaticAddressWithdrawalsV2, error)
	GetWithdrawalWorkflows(context.Context) ([]sqlc.StaticAddressWithdrawalsV2, error)
	UpdateWithdrawalWorkflow(context.Context, sqlc.UpdateWithdrawalWorkflowParams) (int64, error)
	CreateWithdrawalWorkflowDeposit(context.Context, sqlc.CreateWithdrawalWorkflowDepositParams) error
	GetWithdrawalWorkflowDeposits(context.Context, string) ([][]byte, error)
	ClaimWithdrawalWorkflowDeposit(context.Context, sqlc.ClaimWithdrawalWorkflowDepositParams) (int64, error)
	GetWithdrawalWorkflowClaim(context.Context, []byte) (string, error)
	ReleaseWithdrawalWorkflowClaims(context.Context, string) error
	InsertWithdrawalWorkflowUpdate(context.Context, sqlc.InsertWithdrawalWorkflowUpdateParams) error
	GetWithdrawalWorkflowUpdates(context.Context, string) ([]sqlc.StaticAddressWithdrawalV2Update, error)
	GetWithdrawalMigrationDepositUpdate(context.Context, []byte) (sqlc.DepositUpdate, error)
	UpdateWithdrawalWorkflowDepositTx(context.Context, sqlc.UpdateWithdrawalWorkflowDepositTxParams) error
	GetDeposit(context.Context, []byte) (sqlc.Deposit, error)
	AllDeposits(context.Context) ([]sqlc.Deposit, error)
	InsertDepositUpdate(context.Context, sqlc.InsertDepositUpdateParams) error
}

// WorkflowStore adapts withdrawal domain records to the FSM's logical register.
// Claims, deposit mirrors, state and metadata commit in the same transaction.
type WorkflowStore struct{ store *SqlStore }

var _ fsmv2.Store = (*WorkflowStore)(nil)

// WorkflowStore returns the withdrawal-specific durable FSM adapter.
func (s *SqlStore) WorkflowStore() *WorkflowStore { return &WorkflowStore{store: s} }

// Create reserves all inputs and commits signing intent before signing starts.
func (s *WorkflowStore) Create(ctx context.Context, record fsmv2.Record) error {
	data, err := storeDecodeWorkflow(record)
	if err != nil {
		return err
	}
	if record.Revision != 1 || record.State != WorkflowSigning || data.LegacySource != "" ||
		len(data.DepositIDs) == 0 || len(data.Attempts) != 0 || len(data.Inputs) != len(data.DepositIDs) {

		return fmt.Errorf("invalid initial withdrawal intent: %w", fsmv2.ErrInvalidRecord)
	}
	return s.store.baseDB.ExecTx(ctx, loopdb.NewSqlWriteOpts(), func(q Querier) error {
		if err := s.insertWorkflow(ctx, q, record, data); err != nil {
			return err
		}
		for _, id := range data.DepositIDs {
			raw, _ := hex.DecodeString(id)
			row, err := q.GetDeposit(ctx, raw)
			if err != nil {
				return err
			}
			memberships, err := q.GetActiveWithdrawalWorkflowMembership(ctx, raw)
			if err != nil {
				return err
			}
			for _, owner := range memberships {
				if owner != record.ID {
					return fsmv2.ErrConflict
				}
			}
			update, err := q.GetWithdrawalMigrationDepositUpdate(ctx, raw)
			if err != nil {
				return err
			}
			if update.UpdateState != string(deposit.Deposited) || row.ConfirmationHeight <= 0 ||
				row.FinalizedWithdrawalTx.Valid {

				return fmt.Errorf("withdrawal input is not an available confirmed deposit: %w", fsmv2.ErrConflict)
			}
			input, ok := storeWorkflowInput(data, id)
			if !ok || input.Value != row.Amount || input.ConfirmationHeight != row.ConfirmationHeight ||
				input.Outpoint != storeDepositOutpoint(row) || input.Value <= 0 {

				return fmt.Errorf("withdrawal input changed: %w", fsmv2.ErrInvalidRecord)
			}
			affected, err := q.ClaimWithdrawalWorkflowDeposit(ctx, sqlc.ClaimWithdrawalWorkflowDepositParams{
				DepositID: raw, WithdrawalID: record.ID,
			})
			if err != nil {
				return err
			}
			if affected != 1 {
				return fsmv2.ErrConflict
			}
			if err := storeDepositState(ctx, q, raw, workflowReservedDepositState, s.store.clock.Now()); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *WorkflowStore) insertWorkflow(ctx context.Context, q Querier,
	record fsmv2.Record, data WorkflowData) error {

	affected, err := q.CreateWithdrawalWorkflow(ctx, sqlc.CreateWithdrawalWorkflowParams{
		WithdrawalID: record.ID, Version: int64(record.Version), Revision: int64(record.Revision),
		State: string(record.State), WorkflowData: bytes.Clone(record.Data),
		InitiationTime: data.InitiationTime.UTC().Truncate(time.Microsecond),
		LegacySource:   sql.NullString{String: data.LegacySource, Valid: data.LegacySource != ""},
	})
	if err != nil {
		return err
	}
	if affected != 1 {
		return fsmv2.ErrConflict
	}
	for _, id := range data.DepositIDs {
		raw, _ := hex.DecodeString(id)
		if err := q.CreateWithdrawalWorkflowDeposit(ctx, sqlc.CreateWithdrawalWorkflowDepositParams{
			WithdrawalID: record.ID, DepositID: raw,
		}); err != nil {
			return err
		}
	}
	return s.insertUpdate(ctx, q, record)
}

func (s *WorkflowStore) insertUpdate(ctx context.Context, q Querier, record fsmv2.Record) error {
	return q.InsertWithdrawalWorkflowUpdate(ctx, sqlc.InsertWithdrawalWorkflowUpdateParams{
		WithdrawalID: record.ID, Revision: int64(record.Revision), State: string(record.State),
		UpdateTimestamp: s.store.clock.Now().UTC().Truncate(time.Microsecond),
	})
}

// Load returns the exact bytes of the committed withdrawal checkpoint.
func (s *WorkflowStore) Load(ctx context.Context, id string) (fsmv2.Record, error) {
	row, err := s.store.baseDB.GetWithdrawalWorkflow(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return fsmv2.Record{}, fsmv2.ErrNotFound
	}
	if err != nil {
		return fsmv2.Record{}, err
	}
	return storeWorkflowRecord(row)
}

// CompareAndSwap atomically updates the checkpoint, ownership, mirrored deposit
// state and transition history, rejecting stale revisions and version changes.
func (s *WorkflowStore) CompareAndSwap(ctx context.Context, record fsmv2.Record,
	expectedRevision uint64) error {

	data, err := storeDecodeWorkflow(record)
	if err != nil {
		return err
	}
	if expectedRevision == 0 || expectedRevision >= math.MaxInt64 || record.Revision != expectedRevision+1 {
		return fmt.Errorf("withdrawal revision overflow or mismatch: %w", fsmv2.ErrInvalidRecord)
	}
	return s.store.baseDB.ExecTx(ctx, loopdb.NewSqlWriteOpts(), func(q Querier) error {
		previous, err := q.GetWithdrawalWorkflow(ctx, record.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return fsmv2.ErrConflict
		}
		if err != nil {
			return err
		}
		if previous.Revision != int64(expectedRevision) || previous.Version != int64(record.Version) {
			return fsmv2.ErrConflict
		}
		oldRecord, err := storeWorkflowRecord(previous)
		if err != nil {
			return err
		}
		oldData, err := storeDecodeWorkflow(oldRecord)
		if err != nil {
			return err
		}
		if oldRecord.State == WorkflowSucceeded || oldRecord.State == WorkflowFailed {
			return fsmv2.ErrConflict
		}
		if !slices.EqualFunc(oldData.Inputs, data.Inputs, func(a, b WorkflowInput) bool {
			return a.DepositID == b.DepositID && a.Outpoint == b.Outpoint &&
				a.Value == b.Value && a.ConfirmationHeight == b.ConfirmationHeight &&
				bytes.Equal(a.PkScript, b.PkScript)
		}) || oldData.TotalDepositAmount != data.TotalDepositAmount ||
			oldData.InitiationHeight != data.InitiationHeight {

			return fmt.Errorf("withdrawal funding facts changed: %w", fsmv2.ErrInvalidRecord)
		}
		if record.State != WorkflowReconciliation {
			for _, id := range data.DepositIDs {
				raw, _ := hex.DecodeString(id)
				owner, err := q.GetWithdrawalWorkflowClaim(ctx, raw)
				if errors.Is(err, sql.ErrNoRows) {
					return fsmv2.ErrConflict
				}
				if err != nil {
					return err
				}
				if owner != record.ID {
					return fsmv2.ErrConflict
				}
			}
		}
		if !slices.Equal(oldData.DepositIDs, data.DepositIDs) || oldData.LegacySource != data.LegacySource ||
			oldData.LegacyTxID != data.LegacyTxID ||
			!oldData.InitiationTime.Equal(data.InitiationTime) || oldData.RequestedAmount != data.RequestedAmount ||
			!bytes.Equal(oldData.DestinationScript, data.DestinationScript) ||
			len(data.Attempts) < len(oldData.Attempts) {

			return fmt.Errorf("withdrawal identity or escaped intent changed: %w", fsmv2.ErrInvalidRecord)
		}
		for i, attempt := range oldData.Attempts {
			next := data.Attempts[i]
			if attempt.ID != next.ID || attempt.TxID != next.TxID ||
				attempt.FeeRateSatPerKWeight != next.FeeRateSatPerKWeight || !bytes.Equal(attempt.Tx, next.Tx) {

				return fmt.Errorf("withdrawal attempt history changed: %w", fsmv2.ErrInvalidRecord)
			}
		}
		affected, err := q.UpdateWithdrawalWorkflow(ctx, sqlc.UpdateWithdrawalWorkflowParams{
			WithdrawalID: record.ID, Version: int64(record.Version), Revision: int64(record.Revision),
			ExpectedRevision: int64(expectedRevision), State: string(record.State), WorkflowData: bytes.Clone(record.Data),
		})
		if err != nil {
			return err
		}
		if affected != 1 {
			return fsmv2.ErrConflict
		}
		if err := s.mirrorWorkflow(ctx, q, record, data); err != nil {
			return err
		}
		return s.insertUpdate(ctx, q, record)
	})
}

func (s *WorkflowStore) mirrorWorkflow(ctx context.Context, q Querier,
	record fsmv2.Record, data WorkflowData) error {

	var state string
	switch record.State {
	case WorkflowPublishPending, WorkflowMonitoring, WorkflowConfirming, WorkflowFinalizing:
		if len(data.Attempts) == 0 {
			return fmt.Errorf("published withdrawal lacks signed intent: %w", fsmv2.ErrInvalidRecord)
		}
		raw := data.Attempts[len(data.Attempts)-1].Tx
		if (record.State == WorkflowConfirming || record.State == WorkflowFinalizing) && data.WinningTxID != "" {
			for _, a := range data.Attempts {
				if a.TxID == data.WinningTxID {
					raw = a.Tx
					break
				}
			}
		}
		var tx wire.MsgTx
		if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
			return err
		}
		if !storeTransactionInputs(data, &tx) {
			return fmt.Errorf("signed withdrawal inputs changed: %w", fsmv2.ErrInvalidRecord)
		}
		for _, id := range data.DepositIDs {
			depID, _ := hex.DecodeString(id)
			owner, err := q.GetWithdrawalWorkflowClaim(ctx, depID)
			if err != nil {
				return err
			}
			if owner != record.ID {
				return fsmv2.ErrConflict
			}
			if err := q.UpdateWithdrawalWorkflowDepositTx(ctx, sqlc.UpdateWithdrawalWorkflowDepositTxParams{
				DepositID: depID, FinalizedWithdrawalTx: sql.NullString{String: hex.EncodeToString(raw), Valid: true},
			}); err != nil {
				return err
			}
		}
		state = string(deposit.Withdrawing)
	case WorkflowSucceeded:
		if data.ConfirmationHeight == 0 || data.WinningTxID == "" {
			return fmt.Errorf("withdrawal completion lacks chain evidence: %w", fsmv2.ErrInvalidRecord)
		}
		state = string(deposit.Withdrawn)
	case WorkflowFailed:
		if len(data.Attempts) != 0 {
			return fmt.Errorf("cannot release a signed withdrawal: %w", fsmv2.ErrInvalidRecord)
		}
		state = string(deposit.Deposited)
	}
	if state != "" {
		for _, id := range data.DepositIDs {
			depID, _ := hex.DecodeString(id)
			if err := storeDepositState(ctx, q, depID, state, s.store.clock.Now()); err != nil {
				return err
			}
		}
	}
	if record.State == WorkflowSucceeded || record.State == WorkflowFailed {
		return q.ReleaseWithdrawalWorkflowClaims(ctx, record.ID)
	}
	return nil
}

// ListAll returns every durable withdrawal, including terminal historical
// records whose deposit runtime must also be reconciled at startup.
func (s *WorkflowStore) ListAll(ctx context.Context) ([]fsmv2.Record, error) {
	rows, err := s.store.baseDB.GetWithdrawalWorkflows(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]fsmv2.Record, 0, len(rows))
	for _, row := range rows {
		record, err := storeWorkflowRecord(row)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, nil
}

// ListActive includes quarantined legacy intents so recovery cannot overlook
// them. Completed and failed records remain available through withdrawal listing.
func (s *WorkflowStore) ListActive(ctx context.Context) ([]fsmv2.Record, error) {
	records, err := s.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]fsmv2.Record, 0, len(records))
	for _, record := range records {
		if record.State != WorkflowSucceeded && record.State != WorkflowFailed {
			result = append(result, record)
		}
	}
	return result, nil
}

// FindByDeposits requires the complete active membership, including for RBF.
func (s *WorkflowStore) FindByDeposits(ctx context.Context, ids []deposit.ID) (fsmv2.Record, error) {
	target := make([]string, len(ids))
	for i, id := range ids {
		target[i] = hex.EncodeToString(id[:])
	}
	slices.Sort(target)
	if len(target) == 0 || len(slices.Compact(slices.Clone(target))) != len(target) {
		return fsmv2.Record{}, fsmv2.ErrConflict
	}
	records, err := s.ListActive(ctx)
	if err != nil {
		return fsmv2.Record{}, err
	}
	var match *fsmv2.Record
	for _, record := range records {
		data, err := storeDecodeWorkflow(record)
		if err != nil {
			return fsmv2.Record{}, err
		}
		overlap := false
		for _, id := range target {
			if slices.Contains(data.DepositIDs, id) {
				overlap = true
				break
			}
		}
		if !overlap {
			continue
		}
		if !slices.Equal(target, data.DepositIDs) || match != nil {
			return fsmv2.Record{}, fsmv2.ErrConflict
		}
		match = new(record)
	}
	if match == nil {
		return fsmv2.Record{}, fsmv2.ErrNotFound
	}
	return *match, nil
}

func storeDecodeWorkflow(record fsmv2.Record) (WorkflowData, error) {
	var zero WorkflowData
	if record.Version == 0 || record.Revision == 0 || record.Revision > math.MaxInt64 ||
		len(record.Data) == 0 || len(record.Data) > 4<<20 || !storeCanonicalID(record.ID) {

		return zero, fmt.Errorf("withdrawal checkpoint envelope: %w", fsmv2.ErrInvalidRecord)
	}
	data, err := fsmv2.JSONCodec[WorkflowData]().Decode(bytes.Clone(record.Data))
	if err != nil {
		return zero, fmt.Errorf("withdrawal checkpoint data: %w", err)
	}
	if data.ID != record.ID || !slices.IsSorted(data.DepositIDs) {
		return zero, fsmv2.ErrInvalidRecord
	}
	for i, id := range data.DepositIDs {
		if !storeCanonicalID(id) || (i > 0 && data.DepositIDs[i-1] == id) {
			return zero, fsmv2.ErrInvalidRecord
		}
	}
	switch record.State {
	case WorkflowSigning, WorkflowPublishPending, WorkflowMonitoring, WorkflowConfirming, WorkflowFinalizing,
		WorkflowSucceeded, WorkflowFailed, WorkflowReconciliation:
	default:
		return zero, fsmv2.ErrInvalidRecord
	}
	if err := validateWorkflow(record.State, data); err != nil {
		return zero, fmt.Errorf("%w: withdrawal invariant: %w", fsmv2.ErrInvalidRecord, err)
	}
	return data, nil
}

func storeWorkflowRecord(row sqlc.StaticAddressWithdrawalsV2) (fsmv2.Record, error) {
	if row.Version <= 0 || row.Version > math.MaxUint32 || row.Revision <= 0 {
		return fsmv2.Record{}, fsmv2.ErrInvalidRecord
	}
	record := fsmv2.Record{ID: row.WithdrawalID, Version: uint32(row.Version), Revision: uint64(row.Revision),
		State: fsmv2.StateID(row.State), Data: bytes.Clone(row.WorkflowData)}
	_, err := storeDecodeWorkflow(record)
	return record, err
}

func storeCanonicalID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == IdLength && strings.ToLower(id) == id
}

func storeWorkflowInput(data WorkflowData, id string) (WorkflowInput, bool) {
	for _, input := range data.Inputs {
		if input.DepositID == id {
			return input, true
		}
	}
	return WorkflowInput{}, false
}

func storeDepositOutpoint(row sqlc.Deposit) string {
	hash, err := chainhash.NewHash(row.TxHash)
	if err != nil {
		return ""
	}
	return wire.OutPoint{Hash: *hash, Index: uint32(row.OutIndex)}.String()
}

func storeTransactionInputs(data WorkflowData, tx *wire.MsgTx) bool {
	if len(tx.TxIn) != len(data.Inputs) || len(tx.TxIn) == 0 {
		return false
	}
	seen := make(map[string]bool, len(tx.TxIn))
	for _, in := range tx.TxIn {
		key := in.PreviousOutPoint.String()
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	for _, input := range data.Inputs {
		if !seen[input.Outpoint] {
			return false
		}
	}
	return true
}

func storeDepositState(ctx context.Context, q Querier, id []byte, state string, now time.Time) error {
	latest, err := q.GetWithdrawalMigrationDepositUpdate(ctx, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	stamp := now.UTC().Truncate(time.Microsecond)
	if err == nil && !stamp.After(latest.UpdateTimestamp) {
		stamp = latest.UpdateTimestamp.Add(time.Microsecond).UTC().Truncate(time.Microsecond)
	}
	return q.InsertDepositUpdate(ctx, sqlc.InsertDepositUpdateParams{
		DepositID: id, UpdateState: state, UpdateTimestamp: stamp,
	})
}

// getWorkflowWithdrawals reads only the new domain tables. Unrepresentable
// unsigned/pending imports remain durable but are omitted from the existing RPC,
// whose withdrawal response requires a transaction ID.
func (s *SqlStore) getWorkflowWithdrawals(ctx context.Context) ([]Withdrawal, error) {
	var result []Withdrawal
	err := s.baseDB.ExecTx(ctx, loopdb.NewSqlReadOpts(), func(q Querier) error {
		rows, err := q.GetWithdrawalWorkflows(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			record, err := storeWorkflowRecord(row)
			if err != nil {
				return err
			}
			data, err := storeDecodeWorkflow(record)
			if err != nil {
				return err
			}
			txID := data.WinningTxID
			if txID == "" && len(data.Attempts) > 0 {
				txID = data.Attempts[len(data.Attempts)-1].TxID
			}
			if txID == "" {
				txID = data.LegacyTxID
			}
			if txID == "" {
				continue
			}
			hash, err := chainhash.NewHashFromStr(txID)
			if err != nil {
				continue
			}
			var id ID
			raw, _ := hex.DecodeString(data.ID)
			copy(id[:], raw)
			w := Withdrawal{ID: id, TxID: *hash, InitiationTime: data.InitiationTime,
				WithdrawnAmount: btcutil.Amount(data.WithdrawnAmount), ChangeAmount: btcutil.Amount(data.ChangeAmount),
				ConfirmationHeight: int64(data.ConfirmationHeight), TotalDepositAmount: btcutil.Amount(data.TotalDepositAmount)}
			for _, depID := range data.DepositIDs {
				raw, _ := hex.DecodeString(depID)
				depRow, err := q.GetDeposit(ctx, raw)
				if err != nil {
					return err
				}
				update, err := q.GetWithdrawalMigrationDepositUpdate(ctx, raw)
				if err != nil {
					return err
				}
				// Listing needs deposit facts, not an old serialized signing artifact.
				depRow.FinalizedWithdrawalTx = sql.NullString{}
				d, err := deposit.ToDeposit(depRow, update)
				if err != nil {
					return err
				}
				w.Deposits = append(w.Deposits, d)
			}
			result = append(result, w)
		}
		return nil
	})
	return result, err
}
