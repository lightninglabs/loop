package withdraw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	fsmv2 "github.com/lightninglabs/loop/fsm/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
)

type legacyWorkflowPlan struct {
	state fsmv2.StateID
	data  WorkflowData
}

type legacyDepositEvidence struct {
	row    sqlc.Deposit
	update sqlc.DepositUpdate
}

// MigrateLegacy must run before deposit FSMs or withdrawal workers are activated.
// It imports every legacy withdrawal and recoverable orphan deposit
// cluster in one transaction. It never edits withdrawals or withdrawal_deposits.
// A source token makes restart/retry idempotent without overwriting workflows
// that have subsequently advanced. Incomplete or conflicting evidence is kept
// as reconciliation intent; it never authorizes publication or unlocks inputs.
func (s *WorkflowStore) MigrateLegacy(ctx context.Context) error {
	return s.store.baseDB.ExecTx(ctx, loopdb.NewSqlWriteOpts(), func(q Querier) error {
		rows, err := q.GetAllWithdrawals(ctx)
		if err != nil {
			return err
		}
		existing, err := q.GetWithdrawalWorkflows(ctx)
		if err != nil {
			return err
		}
		sources := make(map[string]bool, len(existing))
		covered := make(map[string]bool)
		for _, row := range existing {
			if row.LegacySource.Valid {
				sources[row.LegacySource.String] = true
			}
			record, err := storeWorkflowRecord(row)
			if err != nil {
				return err
			}
			data, err := storeDecodeWorkflow(record)
			if err != nil {
				return err
			}
			for _, id := range data.DepositIDs {
				covered[id] = true
			}
		}
		deposits, err := q.AllDeposits(ctx)
		if err != nil {
			return err
		}
		evidence := make(map[string]legacyDepositEvidence, len(deposits))
		for _, row := range deposits {
			if len(row.DepositID) != deposit.IdLength {
				continue
			}
			update, err := q.GetWithdrawalMigrationDepositUpdate(ctx, row.DepositID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			evidence[hex.EncodeToString(row.DepositID)] = legacyDepositEvidence{row: row, update: update}
		}
		addresses, err := q.AllStaticAddresses(ctx)
		if err != nil {
			return err
		}
		members := make(map[int32][]string, len(rows))
		references := make(map[string]int)
		for _, row := range rows {
			ids, err := q.GetWithdrawalDeposits(ctx, row.WithdrawalID)
			if err != nil {
				return err
			}
			for _, raw := range ids {
				if len(raw) != deposit.IdLength {
					continue
				}
				id := hex.EncodeToString(raw)
				members[row.ID] = append(members[row.ID], id)
				references[id]++
			}
			slices.Sort(members[row.ID])
		}
		plans := make([]legacyWorkflowPlan, 0, len(rows))
		for _, row := range rows {
			source := fmt.Sprintf("legacy-withdrawal:%d", row.ID)
			if sources[source] {
				continue
			}
			data := WorkflowData{ID: hex.EncodeToString(row.WithdrawalID), LegacySource: source,
				DepositIDs: slices.Clone(members[row.ID]), InitiationTime: row.InitiationTime.UTC().Truncate(time.Microsecond),
				TotalDepositAmount: row.TotalDepositAmount, WithdrawnAmount: row.WithdrawnAmount.Int64,
				ChangeAmount: row.ChangeAmount.Int64, LegacyTxID: row.WithdrawalTxID.String}
			if !storeCanonicalID(data.ID) {
				data.ID = legacyWorkflowID(source)
			}
			legacyWorkflowInputs(&data, evidence)
			state := WorkflowReconciliation
			if row.WithdrawalTxID.Valid && len(row.WithdrawalTxID.String) == chainhash.MaxHashStringSize &&
				row.ConfirmationHeight.Valid && row.ConfirmationHeight.Int64 > 0 && row.ConfirmationHeight.Int64 <= math.MaxUint32 {

				if hash, err := chainhash.NewHashFromStr(row.WithdrawalTxID.String); err == nil {
					state = WorkflowSucceeded
					data.WinningTxID = hash.String()
					data.ConfirmationHeight = uint32(row.ConfirmationHeight.Int64)
				}
			}
			for _, id := range data.DepositIDs {
				if references[id] > 1 {
					state = WorkflowReconciliation
				}
			}
			if state != WorkflowSucceeded {
				data.ReconciliationReason = "legacy withdrawal lacks a complete signed transaction and chain result"
				ambiguous := len(data.DepositIDs) == 0
				for _, id := range data.DepositIDs {
					if references[id] > 1 || covered[id] {
						ambiguous = true
					}
				}
				if !ambiguous {
					if err := legacyWorkflowSignedIntent(&data, evidence, addresses, true); err == nil {
						state = WorkflowMonitoring
						data.ReconciliationReason = ""
					} else {
						data.ReconciliationReason = err.Error()
					}
				} else {
					data.ReconciliationReason = "legacy withdrawal associations overlap or are incomplete"
				}
			}
			plans = append(plans, legacyWorkflowPlan{state: state, data: data})
			for _, id := range data.DepositIDs {
				covered[id] = true
			}
		}
		// Unlinked in-flight deposits are grouped only by a full identical signed
		// artifact. A missing/corrupt artifact receives a per-deposit quarantine ID.
		groups := make(map[string][]string)
		for id, item := range evidence {
			if covered[id] || (item.update.UpdateState != string(deposit.Withdrawing) &&
				item.update.UpdateState != workflowReservedDepositState && !item.row.FinalizedWithdrawalTx.Valid) {

				continue
			}
			source := "legacy-deposit:" + id
			if item.row.FinalizedWithdrawalTx.Valid {
				raw, err := hex.DecodeString(item.row.FinalizedWithdrawalTx.String)
				if err == nil {
					var tx wire.MsgTx
					reader := bytes.NewReader(raw)
					if tx.Deserialize(reader) == nil && reader.Len() == 0 {
						source = "legacy-transaction:" + tx.TxHash().String()
					}
				}
			}
			groups[source] = append(groups[source], id)
		}
		for source, ids := range groups {
			if sources[source] {
				continue
			}
			slices.Sort(ids)
			data := WorkflowData{ID: legacyWorkflowID(source), LegacySource: source, DepositIDs: ids,
				InitiationTime: s.store.clock.Now().UTC().Truncate(time.Microsecond)}
			legacyWorkflowInputs(&data, evidence)
			for _, input := range data.Inputs {
				data.TotalDepositAmount += input.Value
			}
			state := WorkflowReconciliation
			ambiguous := false
			for _, id := range ids {
				if references[id] > 0 {
					ambiguous = true
				}
			}
			if !ambiguous {
				if err := legacyWorkflowSignedIntent(&data, evidence, addresses, false); err == nil {
					state = WorkflowMonitoring
				} else {
					data.ReconciliationReason = err.Error()
				}
			} else {
				data.ReconciliationReason = "in-flight deposits conflict with historical withdrawal membership"
			}
			plans = append(plans, legacyWorkflowPlan{state: state, data: data})
		}
		for i := range plans {
			if plans[i].state == WorkflowSucceeded {
				for _, id := range plans[i].data.DepositIDs {
					item, ok := evidence[id]
					if !ok || !legacyCompletedDepositState(item.update.UpdateState) {
						plans[i].state = WorkflowReconciliation
						plans[i].data.ReconciliationReason = "completed legacy withdrawal conflicts with current deposit ownership"
						break
					}
				}
			}
			if err := validateWorkflow(plans[i].state, plans[i].data); err != nil {
				plans[i].state = WorkflowReconciliation
				plans[i].data.ReconciliationReason = "legacy workflow requires reconciliation: " + err.Error()
			}
		}
		// Compute ownership before writing any state so iteration order cannot choose
		// a winner for ambiguous associations.
		claims := make(map[string]int)
		for _, plan := range plans {
			if plan.state != WorkflowSucceeded {
				for _, id := range plan.data.DepositIDs {
					claims[id]++
				}
			}
		}
		for _, plan := range plans {
			for _, id := range plan.data.DepositIDs {
				if claims[id] > 1 && plan.state != WorkflowSucceeded {
					plan.state = WorkflowReconciliation
					plan.data.ReconciliationReason = "multiple legacy intents claim the same deposit"
				}
			}
			if err := validateWorkflow(plan.state, plan.data); err != nil {
				plan.state = WorkflowReconciliation
				plan.data.ReconciliationReason = "legacy workflow requires reconciliation: " + err.Error()
			}
			encoded, err := fsmv2.JSONCodec[WorkflowData]().Encode(plan.data)
			if err != nil {
				return err
			}
			record := fsmv2.Record{ID: plan.data.ID, Version: 1, Revision: 1, State: plan.state, Data: encoded}
			if _, err := storeDecodeWorkflow(record); err != nil {
				return err
			}
			if err := s.insertWorkflow(ctx, q, record, plan.data); err != nil {
				return err
			}
			if plan.state == WorkflowSucceeded {
				for _, id := range plan.data.DepositIDs {
					raw, _ := hex.DecodeString(id)
					if err := reconcileLegacyDepositState(ctx, q, raw, string(deposit.Withdrawn), s.store.clock.Now()); err != nil {
						return err
					}
				}
				continue
			}
			for _, id := range plan.data.DepositIDs {
				raw, _ := hex.DecodeString(id)
				// Keep unrelated loop-in, channel and expiry ownership intact. Normal
				// Create also checks active workflow membership, so a claimless quarantine
				// remains a barrier even if another flow later releases its deposit.
				if !legacyQuarantineDepositState(evidence[id].update.UpdateState) {
					continue
				}
				if claims[id] == 1 && references[id] <= 1 {
					affected, err := q.ClaimWithdrawalWorkflowDeposit(ctx, sqlc.ClaimWithdrawalWorkflowDepositParams{
						DepositID: raw, WithdrawalID: record.ID,
					})
					if err != nil {
						return err
					}
					if affected != 1 {
						return fsmv2.ErrConflict
					}
				}
				state := workflowReservedDepositState
				if plan.state == WorkflowMonitoring {
					state = string(deposit.Withdrawing)
					artifact := plan.data.Attempts[len(plan.data.Attempts)-1].Tx
					if err := q.UpdateWithdrawalWorkflowDepositTx(ctx, sqlc.UpdateWithdrawalWorkflowDepositTxParams{
						DepositID: raw, FinalizedWithdrawalTx: sql.NullString{String: hex.EncodeToString(artifact), Valid: true},
					}); err != nil {
						return err
					}
				}
				if err := storeDepositState(ctx, q, raw, state, s.store.clock.Now()); err != nil {
					return err
				}
			}
		}
		// A previous import may have recorded completion before repairing a legacy
		// deposit mirror. Repair durable mirrors idempotently before runtime reseed.
		for _, row := range existing {
			if fsmv2.StateID(row.State) != WorkflowSucceeded {
				continue
			}
			record, err := storeWorkflowRecord(row)
			if err != nil {
				return err
			}
			data, err := storeDecodeWorkflow(record)
			if err != nil {
				return err
			}
			conflictingOwner := false
			for _, id := range data.DepositIDs {
				raw, _ := hex.DecodeString(id)
				latest, err := q.GetWithdrawalMigrationDepositUpdate(ctx, raw)
				if err != nil {
					return err
				}
				if !legacyCompletedDepositState(latest.UpdateState) {
					conflictingOwner = true
				}
			}
			if conflictingOwner {
				if record.Revision >= math.MaxInt64 {
					return fsmv2.ErrRevisionExhausted
				}
				data.ReconciliationReason = "historical withdrawal conflicts with current deposit ownership"
				encoded, err := fsmv2.JSONCodec[WorkflowData]().Encode(data)
				if err != nil {
					return err
				}
				affected, err := q.UpdateWithdrawalWorkflow(ctx, sqlc.UpdateWithdrawalWorkflowParams{
					WithdrawalID: record.ID, Version: int64(record.Version), ExpectedRevision: int64(record.Revision),
					Revision: int64(record.Revision + 1), State: string(WorkflowReconciliation), WorkflowData: encoded,
				})
				if err != nil {
					return err
				}
				if affected != 1 {
					return fsmv2.ErrConflict
				}
				record.Revision++
				record.State = WorkflowReconciliation
				record.Data = encoded
				if err := s.insertUpdate(ctx, q, record); err != nil {
					return err
				}
				for _, id := range data.DepositIDs {
					raw, _ := hex.DecodeString(id)
					latest, err := q.GetWithdrawalMigrationDepositUpdate(ctx, raw)
					if err != nil {
						return err
					}
					if !legacyQuarantineDepositState(latest.UpdateState) {
						continue
					}
					if _, err := q.GetWithdrawalWorkflowClaim(ctx, raw); err == nil {
						continue
					} else if !errors.Is(err, sql.ErrNoRows) {
						return err
					}
					if err := storeDepositState(ctx, q, raw, workflowReservedDepositState, s.store.clock.Now()); err != nil {
						return err
					}
				}
				continue
			}
			for _, id := range data.DepositIDs {
				raw, _ := hex.DecodeString(id)
				state := string(deposit.Withdrawn)
				if references[id] > 1 {
					state = workflowReservedDepositState
				}
				if err := reconcileLegacyDepositState(ctx, q, raw, state, s.store.clock.Now()); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func legacyCompletedDepositState(state string) bool {
	return state == string(deposit.Deposited) || state == string(deposit.Withdrawing) ||
		state == string(deposit.Withdrawn)
}

func legacyQuarantineDepositState(state string) bool {
	return legacyCompletedDepositState(state) || state == workflowReservedDepositState
}

func reconcileLegacyDepositState(ctx context.Context, q Querier, id []byte, state string, now time.Time) error {
	if _, err := q.GetWithdrawalWorkflowClaim(ctx, id); err == nil {
		return fsmv2.ErrConflict
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	latest, err := q.GetWithdrawalMigrationDepositUpdate(ctx, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && latest.UpdateState == state {
		count, err := q.CountWithdrawalDepositUpdatesAtTime(ctx, sqlc.CountWithdrawalDepositUpdatesAtTimeParams{
			DepositID: id, UpdateTimestamp: latest.UpdateTimestamp,
		})
		if err != nil {
			return err
		}
		if count == 1 {
			return nil
		}
	}
	return storeDepositState(ctx, q, id, state, now)
}

func legacyWorkflowID(source string) string {
	hash := sha256.Sum256([]byte("loop-static-withdrawal-v2:" + source))
	return hex.EncodeToString(hash[:])
}

func legacyWorkflowInputs(data *WorkflowData, evidence map[string]legacyDepositEvidence) {
	for _, id := range data.DepositIDs {
		item, ok := evidence[id]
		if !ok {
			continue
		}
		data.Inputs = append(data.Inputs, WorkflowInput{DepositID: id, Outpoint: storeDepositOutpoint(item.row),
			Value: item.row.Amount, ConfirmationHeight: item.row.ConfirmationHeight})
		if item.row.ConfirmationHeight > 0 && item.row.ConfirmationHeight <= math.MaxUint32 &&
			(data.InitiationHeight == 0 || uint32(item.row.ConfirmationHeight) < data.InitiationHeight) {

			data.InitiationHeight = uint32(item.row.ConfirmationHeight)
		}
	}
}

func legacyWorkflowSignedIntent(data *WorkflowData, evidence map[string]legacyDepositEvidence,
	addresses []sqlc.StaticAddress, explicitMembership bool) error {

	if len(data.DepositIDs) == 0 || len(data.Inputs) != len(data.DepositIDs) {
		return errors.New("legacy signed intent has incomplete deposit membership")
	}
	var raw []byte
	for _, id := range data.DepositIDs {
		item := evidence[id]
		safeState := item.update.UpdateState == string(deposit.Withdrawing) ||
			item.update.UpdateState == string(deposit.Withdrawn)
		if explicitMembership && item.update.UpdateState == string(deposit.Deposited) {
			safeState = true
		}
		if !safeState {
			return errors.New("legacy signed intent has incompatible deposit state")
		}
		if !item.row.FinalizedWithdrawalTx.Valid {
			// A legacy membership row plus one verified full transaction binds every
			// input, even if a crash interrupted per-deposit transaction mirroring.
			if explicitMembership {
				continue
			}
			return errors.New("legacy signed transaction is missing")
		}
		candidate, err := hex.DecodeString(item.row.FinalizedWithdrawalTx.String)
		if err != nil {
			return errors.New("legacy signed transaction is malformed")
		}
		if raw == nil {
			raw = candidate
		} else if !bytes.Equal(raw, candidate) {
			return errors.New("legacy deposits contain different signed transactions")
		}
	}
	var tx wire.MsgTx
	reader := bytes.NewReader(raw)
	if tx.Deserialize(reader) != nil || reader.Len() != 0 || !storeTransactionInputs(*data, &tx) ||
		len(tx.TxOut) == 0 || len(tx.TxOut) > 2 {

		return errors.New("legacy signed transaction does not cover exactly its deposit inputs")
	}
	var totalIn, totalOut int64
	for _, input := range data.Inputs {
		if input.Value <= 0 || input.Value > btcMaxMoney || input.ConfirmationHeight <= 0 {
			return errors.New("legacy input facts are incomplete")
		}
		totalIn += input.Value
		if totalIn > btcMaxMoney {
			return errors.New("legacy input value is out of range")
		}
	}
	for _, output := range tx.TxOut {
		if output.Value < 0 || output.Value > btcMaxMoney {
			return errors.New("legacy output value is out of range")
		}
		totalOut += output.Value
	}
	if totalOut >= totalIn || totalOut > btcMaxMoney || tx.TxOut[0].Value <= 0 || data.TotalDepositAmount != totalIn {
		return errors.New("legacy transaction fee or output amounts are invalid")
	}
	// Deposits do not persist their funding script, so an address association is
	// accepted only when a stored Taproot script validates every input witness.
	var matching []byte
	for _, address := range addresses {
		if !txscript.IsPayToTaproot(address.Pkscript) {
			continue
		}
		if legacyWorkflowWitnesses(data, &tx, address.Pkscript) {
			if matching != nil {
				return errors.New("legacy funding script association is ambiguous")
			}
			matching = bytes.Clone(address.Pkscript)
		}
	}
	if matching == nil {
		return errors.New("legacy funding script or signatures cannot be verified")
	}
	ordered := make([]WorkflowInput, 0, len(data.Inputs))
	for _, in := range tx.TxIn {
		for _, source := range data.Inputs {
			if source.Outpoint == in.PreviousOutPoint.String() {
				source.PkScript = bytes.Clone(matching)
				ordered = append(ordered, source)
				break
			}
		}
	}
	data.Inputs = ordered
	if len(tx.TxOut) == 2 && !bytes.Equal(tx.TxOut[1].PkScript, matching) {
		return errors.New("legacy change output is not owned")
	}
	weight := int64(tx.SerializeSizeStripped()*3 + tx.SerializeSize())
	data.FeeRateSatPerKWeight = max(int64(1), (totalIn-totalOut)*1000/weight)
	data.Attempt = 1
	data.Attempts = []WorkflowAttempt{{ID: 1, Tx: bytes.Clone(raw), TxID: tx.TxHash().String(),
		FeeRateSatPerKWeight: data.FeeRateSatPerKWeight,
	}}
	data.DestinationScript = bytes.Clone(tx.TxOut[0].PkScript)
	data.WithdrawnAmount = tx.TxOut[0].Value
	data.ChangeAmount = 0
	data.RequestedAmount = 0
	if len(tx.TxOut) == 2 {
		data.ChangeAmount = tx.TxOut[1].Value
		// A verified owned change output proves a partial withdrawal; preserve the
		// approved recipient amount when a later fee bump omits the amount.
		data.RequestedAmount = tx.TxOut[0].Value
	}
	unsigned := tx.Copy()
	for _, in := range unsigned.TxIn {
		in.Witness = nil
		in.SignatureScript = nil
	}
	var buffer bytes.Buffer
	if err := unsigned.Serialize(&buffer); err != nil {
		return err
	}
	data.UnsignedTx = buffer.Bytes()
	return nil
}

const btcMaxMoney int64 = 21_000_000 * 100_000_000

func legacyWorkflowWitnesses(data *WorkflowData, tx *wire.MsgTx, script []byte) bool {
	outputs := make(map[wire.OutPoint]*wire.TxOut, len(data.Inputs))
	for _, input := range data.Inputs {
		outpoint, err := wire.NewOutPointFromString(input.Outpoint)
		if err != nil {
			return false
		}
		outputs[*outpoint] = &wire.TxOut{Value: input.Value, PkScript: script}
	}
	fetcher := txscript.NewMultiPrevOutFetcher(outputs)
	hashes := txscript.NewTxSigHashes(tx, fetcher)
	for i, input := range tx.TxIn {
		prev := outputs[input.PreviousOutPoint]
		if prev == nil {
			return false
		}
		engine, err := txscript.NewEngine(script, tx, i, txscript.StandardVerifyFlags, nil, hashes, prev.Value, fetcher)
		if err != nil || engine.Execute() != nil {
			return false
		}
	}
	return true
}
