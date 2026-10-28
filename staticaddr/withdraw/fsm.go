package withdraw

import (
	"bytes"
	"context"
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
)

// Withdrawal workflow states describe durable obligations, not RPC execution.
const (
	WorkflowSigning        fsmv2.StateID = "Signing"
	WorkflowPublishPending fsmv2.StateID = "PublishPending"
	WorkflowMonitoring     fsmv2.StateID = "Monitoring"
	WorkflowConfirming     fsmv2.StateID = "Confirming"
	WorkflowFinalizing     fsmv2.StateID = "Finalizing"
	WorkflowSucceeded      fsmv2.StateID = "Succeeded"
	WorkflowFailed         fsmv2.StateID = "Failed"
	WorkflowReconciliation fsmv2.StateID = "NeedsReconciliation"
)

const (
	workflowSigned    fsmv2.Event = "Signed"
	workflowPublished fsmv2.Event = "Published"
	workflowSpend     fsmv2.Event = "SpendObserved"
	workflowConfirmed fsmv2.Event = "Confirmed"
	workflowFinalize  fsmv2.Event = "Finalized"
	workflowBump      fsmv2.Event = "FeeBump"
	workflowAbort     fsmv2.Event = "SigningFailed"
	workflowResume    fsmv2.Event = "ResumeMonitoring"
	workflowReorg     fsmv2.Event = "Reorg"
	workflowConflict  fsmv2.Event = "UnexpectedSpend"
)

// WorkflowInput binds a deposit to its approved transaction input and prevout.
type WorkflowInput struct {
	DepositID          string
	Outpoint           string
	Value              int64
	PkScript           []byte
	ConfirmationHeight int64
}

// WorkflowAttempt retains every signed candidate, including replaced ones.
// Signer sessions and private nonces must never be persisted.
type WorkflowAttempt struct {
	ID                   uint64
	Tx                   []byte
	TxID                 string
	FeeRateSatPerKWeight int64
}

// WorkflowData is the withdrawal-specific durable intent and recovery data.
// Legacy summaries without reconstructable artifacts are retained as history or
// explicitly quarantined; their presence never authorizes another publication.
type WorkflowData struct {
	ID                    string
	DepositIDs            []string
	Inputs                []WorkflowInput
	DestinationScript     []byte
	DestinationAddress    string
	RequestedAmount       int64
	TotalDepositAmount    int64
	FeeRateSatPerKWeight  int64
	InitiationHeight      uint32
	InitiationTime        time.Time
	Attempt               uint64
	UnsignedTx            []byte
	Attempts              []WorkflowAttempt
	WinningTxID           string
	WithdrawnAmount       int64
	ChangeAmount          int64
	ConfirmationHeight    uint32
	ConfirmationBlockHash string
	LegacySource          string
	LegacyTxID            string
	ReconciliationReason  string
}

type workflowEvent struct {
	Attempt              uint64
	Tx                   []byte
	TxID                 string
	Height               uint32
	BlockHash            string
	UnsignedTx           []byte
	FeeRateSatPerKWeight int64
	RequestedAmount      int64
	Reason               string
}

func canonicalID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == IdLength &&
		hex.EncodeToString(decoded) == id
}

func decodeWorkflowTx(raw []byte) (*wire.MsgTx, error) {
	reader := bytes.NewReader(raw)
	tx := wire.NewMsgTx(2)
	if err := tx.Deserialize(reader); err != nil {
		return nil, err
	}
	if reader.Len() != 0 {
		return nil, errors.New("trailing withdrawal transaction bytes")
	}
	return tx, nil
}

func encodeWorkflowTx(tx *wire.MsgTx) ([]byte, error) {
	var raw bytes.Buffer
	if err := tx.Serialize(&raw); err != nil {
		return nil, err
	}
	return raw.Bytes(), nil
}

func validateWorkflow(state fsmv2.StateID, data WorkflowData) error {
	if !canonicalID(data.ID) {
		return errors.New("invalid withdrawal identity or accounting")
	}
	seen := make(map[string]struct{}, len(data.DepositIDs))
	for _, id := range data.DepositIDs {
		if !canonicalID(id) {
			return errors.New("invalid withdrawal member")
		}
		if _, ok := seen[id]; ok {
			return errors.New("duplicate withdrawal member")
		}
		seen[id] = struct{}{}
	}
	if !slices.IsSorted(data.DepositIDs) {
		return errors.New("withdrawal members are not ordered")
	}
	if state == WorkflowReconciliation {
		if data.ReconciliationReason == "" {
			return errors.New("missing withdrawal reconciliation reason")
		}
		return nil
	}
	if data.InitiationTime.IsZero() || data.TotalDepositAmount < 0 ||
		data.WithdrawnAmount < 0 || data.ChangeAmount < 0 {

		return errors.New("invalid withdrawal accounting or initiation time")
	}
	if state == WorkflowSucceeded && data.LegacySource != "" && len(data.Attempts) == 0 {
		if len(data.Inputs) == 0 || len(data.Inputs) != len(data.DepositIDs) ||
			data.WithdrawnAmount <= 0 || data.WithdrawnAmount > data.TotalDepositAmount ||
			data.ChangeAmount > data.TotalDepositAmount-data.WithdrawnAmount {

			return errors.New("inconsistent historical withdrawal accounting")
		}
		var total int64
		members := make(map[string]bool, len(data.Inputs))
		for _, in := range data.Inputs {
			if _, ok := seen[in.DepositID]; !ok || members[in.DepositID] ||
				in.Value <= 0 || in.Value > math.MaxInt64-total {

				return errors.New("inconsistent historical withdrawal membership")
			}
			members[in.DepositID] = true
			total += in.Value
		}
		if total != data.TotalDepositAmount {
			return errors.New("historical withdrawal input total mismatch")
		}
		if data.WinningTxID == "" || data.ConfirmationHeight == 0 {
			return errors.New("incomplete historical withdrawal")
		}
		hash, err := chainhash.NewHashFromStr(data.WinningTxID)
		if err != nil || hash.String() != data.WinningTxID {
			return errors.New("invalid historical withdrawal transaction id")
		}
		return nil
	}
	if len(data.Inputs) == 0 || len(data.Inputs) != len(seen) ||
		data.Attempt == 0 || len(data.DestinationScript) == 0 ||
		(data.DestinationAddress == "" && data.LegacySource == "") ||
		data.FeeRateSatPerKWeight <= 0 ||
		data.RequestedAmount < 0 {

		return errors.New("incomplete withdrawal intent")
	}
	var total int64
	outpoints := make(map[string]struct{}, len(data.Inputs))
	members := make(map[string]struct{}, len(data.Inputs))
	for _, in := range data.Inputs {
		if _, ok := seen[in.DepositID]; !ok {
			return errors.New("input does not belong to withdrawal")
		}
		if _, ok := members[in.DepositID]; ok {
			return errors.New("duplicate withdrawal input member")
		}
		members[in.DepositID] = struct{}{}
		if _, ok := outpoints[in.Outpoint]; ok {
			return errors.New("duplicate withdrawal outpoint")
		}
		outpoints[in.Outpoint] = struct{}{}
		if in.Value <= 0 || in.Value > math.MaxInt64-total ||
			in.ConfirmationHeight <= 0 || len(in.PkScript) == 0 {

			return errors.New("invalid withdrawal prevout")
		}
		total += in.Value
	}
	if total != data.TotalDepositAmount {
		return errors.New("withdrawal input accounting mismatch")
	}
	if state == WorkflowSigning {
		if _, err := validateWorkflowCandidate(data, data.UnsignedTx, false); err != nil {
			return err
		}
	}
	ids := make(map[uint64]struct{}, len(data.Attempts))
	for _, candidate := range data.Attempts {
		if candidate.ID == 0 || candidate.ID > data.Attempt {
			return errors.New("invalid withdrawal attempt generation")
		}
		if _, ok := ids[candidate.ID]; ok {
			return errors.New("duplicate withdrawal attempt")
		}
		ids[candidate.ID] = struct{}{}
		tx, err := validateWorkflowCandidate(data, candidate.Tx, true)
		if err != nil {
			return err
		}
		if tx.TxHash().String() != candidate.TxID {
			return errors.New("withdrawal transaction id mismatch")
		}
	}
	if state == WorkflowFailed {
		if len(data.Attempts) != 0 {
			return errors.New("cannot release a signed withdrawal")
		}
		return nil
	}
	if state != WorkflowSigning && len(data.Attempts) == 0 {
		return errors.New("withdrawal has no durable signed candidate")
	}
	if state == WorkflowConfirming || state == WorkflowFinalizing ||
		state == WorkflowSucceeded {

		if workflowCandidate(data, data.WinningTxID) == nil {
			return errors.New("unknown winning withdrawal attempt")
		}
	}
	if state == WorkflowFinalizing || state == WorkflowSucceeded {
		winner := workflowCandidate(data, data.WinningTxID)
		if winner == nil {
			return errors.New("missing withdrawal winner")
		}
		tx, err := decodeWorkflowTx(winner.Tx)
		if err != nil {
			return err
		}
		var change int64
		if len(tx.TxOut) == 2 {
			change = tx.TxOut[1].Value
		}
		if data.WithdrawnAmount != tx.TxOut[0].Value || data.ChangeAmount != change {
			return errors.New("withdrawal accounting differs from confirmed transaction")
		}
		if data.ConfirmationHeight == 0 || !canonicalWorkflowHash(data.ConfirmationBlockHash) {
			return errors.New("missing withdrawal confirmation evidence")
		}
	}
	return nil
}

func validateWorkflowCandidate(data WorkflowData, raw []byte,
	signed bool) (*wire.MsgTx, error) {

	tx, err := decodeWorkflowTx(raw)
	if err != nil {
		return nil, err
	}
	if len(tx.TxIn) != len(data.Inputs) || len(tx.TxOut) == 0 ||
		len(tx.TxOut) > 2 || tx.Version != 2 || tx.LockTime != 0 {

		return nil, errors.New("withdrawal transaction shape mismatch")
	}
	prevouts := make(map[wire.OutPoint]*wire.TxOut, len(data.Inputs))
	var total int64
	for i, in := range data.Inputs {
		if tx.TxIn[i].PreviousOutPoint.String() != in.Outpoint {
			return nil, errors.New("withdrawal input order mismatch")
		}
		if len(tx.TxIn[i].SignatureScript) != 0 ||
			(tx.TxIn[i].Sequence != 0 && tx.TxIn[i].Sequence != wire.MaxTxInSequenceNum-2) {

			return nil, errors.New("unexpected withdrawal input script")
		}
		prevouts[tx.TxIn[i].PreviousOutPoint] = &wire.TxOut{
			Value: in.Value, PkScript: in.PkScript,
		}
		total += in.Value
	}
	if !bytes.Equal(tx.TxOut[0].PkScript, data.DestinationScript) ||
		tx.TxOut[0].Value <= 0 || (data.RequestedAmount > 0 &&
		tx.TxOut[0].Value != data.RequestedAmount) {

		return nil, errors.New("withdrawal destination or amount mismatch")
	}
	var outputs int64
	for _, out := range tx.TxOut {
		if out.Value < 0 || out.Value > total-outputs {
			return nil, errors.New("invalid withdrawal output value")
		}
		outputs += out.Value
	}
	// Change always returns to the same static address as the funding inputs.
	if len(tx.TxOut) == 2 &&
		!bytes.Equal(tx.TxOut[1].PkScript, data.Inputs[0].PkScript) {

		return nil, errors.New("withdrawal change is not owned")
	}
	if total <= outputs {
		return nil, errors.New("withdrawal has no transaction fee")
	}
	if !signed {
		for _, in := range tx.TxIn {
			if len(in.Witness) != 0 {
				return nil, errors.New("unsigned withdrawal contains witnesses")
			}
		}
		return tx, nil
	}
	fetcher := txscript.NewMultiPrevOutFetcher(prevouts)
	hashes := txscript.NewTxSigHashes(tx, fetcher)
	for i, in := range data.Inputs {
		engine, err := txscript.NewEngine(in.PkScript, tx, i,
			txscript.StandardVerifyFlags, nil, hashes, in.Value, fetcher)
		if err != nil {
			return nil, err
		}
		if err := engine.Execute(); err != nil {
			return nil, fmt.Errorf("invalid withdrawal witness: %w", err)
		}
	}
	return tx, nil
}

func canonicalWorkflowHash(value string) bool {
	hash, err := chainhash.NewHashFromStr(value)
	return err == nil && len(value) == chainhash.MaxHashStringSize && hash.String() == value
}

func workflowCandidate(data WorkflowData, txid string) *WorkflowAttempt {
	for i := range data.Attempts {
		if data.Attempts[i].TxID == txid {
			return &data.Attempts[i]
		}
	}
	return nil
}

func (m *Manager) workflowConfig(id string) fsmv2.Config[WorkflowData, workflowEvent] {
	states := make(map[fsmv2.StateID]fsmv2.State[WorkflowData, workflowEvent])
	for _, stateID := range []fsmv2.StateID{
		WorkflowSigning, WorkflowPublishPending, WorkflowMonitoring,
		WorkflowConfirming, WorkflowFinalizing, WorkflowSucceeded,
		WorkflowFailed, WorkflowReconciliation,
	} {
		state := fsmv2.State[WorkflowData, workflowEvent]{
			Validate: func(data WorkflowData) error {
				return validateWorkflow(stateID, data)
			},
		}
		if stateID == WorkflowSucceeded || stateID == WorkflowFailed {
			state.Terminal = true
		} else {
			state.Enter = m.enterWorkflow
			state.Restore = m.restoreWorkflow
			state.Transitions = make(map[fsmv2.Event]fsmv2.Transition[WorkflowData, workflowEvent])
		}
		states[stateID] = state
	}
	add := func(from fsmv2.StateID, event fsmv2.Event, to fsmv2.StateID) {
		states[from].Transitions[event] = fsmv2.Transition[WorkflowData, workflowEvent]{
			To: to, Gate: workflowGate(event), Update: workflowUpdate(event),
		}
	}
	add(WorkflowSigning, workflowSigned, WorkflowPublishPending)
	add(WorkflowSigning, workflowAbort, WorkflowFailed)
	add(WorkflowSigning, workflowResume, WorkflowMonitoring)
	add(WorkflowPublishPending, workflowPublished, WorkflowMonitoring)
	add(WorkflowMonitoring, workflowBump, WorkflowSigning)
	add(WorkflowConfirming, workflowConfirmed, WorkflowFinalizing)
	add(WorkflowFinalizing, workflowFinalize, WorkflowSucceeded)
	add(WorkflowConfirming, workflowReorg, WorkflowMonitoring)
	add(WorkflowFinalizing, workflowReorg, WorkflowMonitoring)
	for _, state := range []fsmv2.StateID{
		WorkflowSigning, WorkflowPublishPending, WorkflowMonitoring,
		WorkflowReconciliation,
	} {
		add(state, workflowSpend, WorkflowConfirming)
		add(state, workflowConflict, WorkflowReconciliation)
	}
	return fsmv2.Config[WorkflowData, workflowEvent]{
		ID: id, Version: 1, Store: &ownedWorkflowStore{manager: m, inner: m.cfg.Store.WorkflowStore()},
		DataCodec:  fsmv2.JSONCodec[WorkflowData](),
		EventCodec: fsmv2.JSONCodec[workflowEvent](),
		States:     states, MaxBytes: 4 << 20,
	}
}

func workflowGate(event fsmv2.Event) fsmv2.Gate[WorkflowData, workflowEvent] {
	return func(_ context.Context, view fsmv2.Snapshot[WorkflowData],
		msg workflowEvent) error {

		data := view.Data
		switch event {
		case workflowSigned:
			if msg.Attempt != data.Attempt {
				return errors.New("stale withdrawal signing result")
			}
			tx, err := validateWorkflowCandidate(data, msg.Tx, true)
			if err != nil {
				return err
			}
			plan, err := decodeWorkflowTx(data.UnsignedTx)
			if err != nil || tx.TxHash() != plan.TxHash() {
				return errors.New("signed withdrawal differs from approved plan")
			}
		case workflowBump:
			if msg.Attempt != data.Attempt+1 || data.Attempt == math.MaxUint64 ||
				data.WinningTxID != "" ||
				msg.FeeRateSatPerKWeight <= data.FeeRateSatPerKWeight ||
				msg.RequestedAmount != data.RequestedAmount {

				return errors.New("invalid withdrawal replacement request")
			}
			candidate := data
			candidate.UnsignedTx = msg.UnsignedTx
			candidate.FeeRateSatPerKWeight = msg.FeeRateSatPerKWeight
			if _, err := validateWorkflowCandidate(candidate, msg.UnsignedTx, false); err != nil {
				return err
			}
			if len(data.Attempts) == 0 {
				return errors.New("replacement has no previous attempt")
			}
			previous, err := decodeWorkflowTx(data.Attempts[len(data.Attempts)-1].Tx)
			if err != nil {
				return err
			}
			next, err := decodeWorkflowTx(msg.UnsignedTx)
			if err != nil {
				return err
			}
			var oldOutputs, newOutputs int64
			for _, out := range previous.TxOut {
				oldOutputs += out.Value
			}
			for _, out := range next.TxOut {
				newOutputs += out.Value
			}
			if newOutputs >= oldOutputs {
				return errors.New("replacement absolute fee must increase")
			}
		case workflowPublished:
			candidate := workflowCandidate(data, msg.TxID)
			if msg.Attempt != data.Attempt || candidate == nil || candidate.ID != msg.Attempt {
				return errors.New("stale withdrawal publication result")
			}
		case workflowSpend:
			candidate := workflowCandidate(data, msg.TxID)
			if candidate == nil || !bytes.Equal(candidate.Tx, msg.Tx) {
				return errors.New("unrecognized withdrawal spender")
			}
		case workflowConfirmed:
			if msg.TxID != data.WinningTxID || msg.Height == 0 ||
				!canonicalWorkflowHash(msg.BlockHash) {

				return errors.New("invalid withdrawal confirmation")
			}
		case workflowFinalize:
			if data.WinningTxID == "" || data.ConfirmationHeight == 0 ||
				msg.TxID != data.WinningTxID || msg.Height != data.ConfirmationHeight ||
				msg.BlockHash != data.ConfirmationBlockHash {

				return errors.New("withdrawal finalization evidence is stale")
			}
		case workflowAbort:
			if msg.Attempt != data.Attempt || len(data.Attempts) != 0 {
				return errors.New("cannot release an uncertain withdrawal")
			}
		case workflowResume:
			if msg.Attempt != data.Attempt || len(data.Attempts) == 0 {
				return errors.New("no prior withdrawal candidate to resume")
			}
		case workflowReorg:
			if msg.TxID != data.WinningTxID {
				return errors.New("stale withdrawal chain observation")
			}
		case workflowConflict:
			if msg.Reason == "" {
				return errors.New("missing withdrawal conflict evidence")
			}
		}
		return nil
	}
}

func workflowUpdate(event fsmv2.Event) fsmv2.Update[WorkflowData, workflowEvent] {
	return func(_ context.Context, view fsmv2.Snapshot[WorkflowData],
		msg workflowEvent) (WorkflowData, error) {

		data := view.Data
		switch event {
		case workflowSigned:
			tx, err := decodeWorkflowTx(msg.Tx)
			if err != nil {
				return WorkflowData{}, err
			}
			data.Attempts = append(data.Attempts, WorkflowAttempt{
				ID: data.Attempt, Tx: bytes.Clone(msg.Tx), TxID: tx.TxHash().String(),
				FeeRateSatPerKWeight: data.FeeRateSatPerKWeight,
			})
		case workflowBump:
			data.Attempt = msg.Attempt
			data.UnsignedTx = bytes.Clone(msg.UnsignedTx)
			data.FeeRateSatPerKWeight = msg.FeeRateSatPerKWeight
		case workflowSpend:
			data.WinningTxID = msg.TxID
		case workflowConfirmed:
			data.ConfirmationHeight = msg.Height
			data.ConfirmationBlockHash = msg.BlockHash
			candidate := workflowCandidate(data, data.WinningTxID)
			if candidate == nil {
				return WorkflowData{}, errors.New("missing withdrawal winner")
			}
			tx, err := decodeWorkflowTx(candidate.Tx)
			if err != nil {
				return WorkflowData{}, err
			}
			data.WithdrawnAmount = tx.TxOut[0].Value
			if len(tx.TxOut) == 2 {
				data.ChangeAmount = tx.TxOut[1].Value
			}
		case workflowResume:
			last := data.Attempts[len(data.Attempts)-1]
			if last.FeeRateSatPerKWeight > 0 {
				data.FeeRateSatPerKWeight = last.FeeRateSatPerKWeight
			}
		case workflowReorg:
			data.WinningTxID = ""
			data.ConfirmationHeight = 0
			data.ConfirmationBlockHash = ""
		case workflowConflict:
			data.ReconciliationReason = msg.Reason
		}
		return data, nil
	}
}
