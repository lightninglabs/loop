package withdraw

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	fsmv2 "github.com/lightninglabs/loop/fsm/v2"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/staticutil"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrWithdrawingMixedDeposits is returned when a withdrawal is
	// requested for deposits in different states.
	ErrWithdrawingMixedDeposits = errors.New("need to withdraw deposits " +
		"having the same state, either all deposited or all " +
		"withdrawing")

	// ErrDiffPreviousWithdrawalTx signals that the user selected new
	// deposits that have different previous withdrawal transactions.
	ErrDiffPreviousWithdrawalTx = errors.New("can't bump fee for " +
		"deposits with different previous withdrawal tx hash")

	// ErrMissingPreviousWithdrawn is returned when the user tries to bump
	// the fee for a subset of previously selected deposits to withdraw.
	ErrMissingPreviousWithdrawn = errors.New("can't bump fee for subset " +
		"of clustered deposits")

	// ErrMissingFinalizedTx is returned if previously withdrawn deposits
	// don't have a finalized withdrawal tx attached.
	ErrMissingFinalizedTx = errors.New("deposit does not have a " +
		"finalized withdrawal tx, can't bump fee")

	// ErrDiffWithdrawalAddress is returned when the user tries to bump the
	// fee of a withdrawal to a different withdrawal address.
	ErrDiffWithdrawalAddress = errors.New("can't bump fee to a " +
		"different withdrawal address")

	// MinConfs is the minimum number of confirmations we require for a
	// deposit to be considered withdrawn.
	MinConfs int32 = 3

	// Is the default confirmation target for the fee estimation of the
	// withdrawal transaction.
	defaultConfTarget int32 = 3
)

// ManagerConfig preserves the client and server RPC dependencies of withdrawals.
type ManagerConfig struct {
	StaticAddressServerClient staticaddressrpc.StaticAddressServerClient
	AddressManager            AddressManager
	DepositManager            DepositManager
	WalletKit                 lndclient.WalletKitClient
	ChainParams               *chaincfg.Params
	ChainNotifier             lndclient.ChainNotifierClient
	Signer                    lndclient.SignerClient
	Store                     *SqlStore
}

type newWithdrawalRequest struct {
	outpoints   []wire.OutPoint
	respChan    chan *newWithdrawalResponse
	destAddr    string
	satPerVbyte int64
	amount      int64
}

type newWithdrawalResponse struct {
	txHash            string
	withdrawalAddress string
	err               error
}

type workflowResult struct {
	id      string
	event   fsmv2.Event
	message workflowEvent
	err     error
}

type withdrawalRuntime struct {
	machine            *fsmv2.Machine[WorkflowData, workflowEvent]
	signAttempt        uint64
	signCancel         context.CancelFunc
	publishAttempt     uint64
	publishCancel      context.CancelFunc
	publishWake        chan struct{}
	confirmationKey    string
	confirmationCancel context.CancelFunc
	monitors           map[string]context.CancelFunc
	waiters            []chan *newWithdrawalResponse
	needsRecovery      bool
}

// Manager owns withdrawal workflows and supervised runtime workers. All FSM
// events are serialized by Run; worker results never mutate business state.
type Manager struct {
	cfg                      *ManagerConfig
	newWithdrawalRequestChan chan newWithdrawalRequest
	exitChan                 chan struct{}
	initiationHeight         atomic.Uint32
	results                  chan workflowResult
	workflows                map[string]*withdrawalRuntime
	lifecycle                context.Context
	wg                       sync.WaitGroup
}

// NewManager creates the manager without starting workers or signing sessions.
func NewManager(cfg *ManagerConfig, currentHeight uint32) (*Manager, error) {
	if currentHeight == 0 {
		return nil, fmt.Errorf("invalid current height %d", currentHeight)
	}
	m := &Manager{
		cfg: cfg, newWithdrawalRequestChan: make(chan newWithdrawalRequest),
		exitChan: make(chan struct{}), results: make(chan workflowResult, 128),
		workflows: make(map[string]*withdrawalRuntime),
	}
	m.initiationHeight.Store(currentHeight)
	return m, nil
}

// Run restores obligations and supervises workflows. The caller must import
// legacy records before starting the deposit manager or any other input owner.
func (m *Manager) Run(ctx context.Context, initChan chan struct{}) error {
	ctx, cancel := context.WithCancel(ctx)
	m.lifecycle = ctx
	defer func() { cancel(); m.wg.Wait(); close(m.exitChan) }()
	// Reconcile the imported historical and active facts before accepting
	// commands. The import itself ran before deposit FSM activation.
	records, err := m.cfg.Store.WorkflowStore().ListAll(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		data, err := fsmv2.JSONCodec[WorkflowData]().Decode(record.Data)
		if err != nil {
			return err
		}
		points, err := workflowOutpoints(data)
		if err != nil {
			return err
		}
		if len(points) > 0 {
			if err := m.cfg.DepositManager.ReconcileWithdrawal(ctx, points); err != nil {
				return fmt.Errorf("reconcile migrated withdrawal %s: %w", record.ID, err)
			}
		}
	}
	blocks, blockErrors, err := m.cfg.ChainNotifier.RegisterBlockEpochNtfn(ctx)
	if err != nil {
		return err
	}
	if err := m.recoverWithdrawals(ctx); err != nil {
		return err
	}
	close(initChan)
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()
	for {
		select {
		case request := <-m.newWithdrawalRequestChan:
			if err := m.startWithdrawal(ctx, request); err != nil {
				request.respChan <- &newWithdrawalResponse{err: err}
			}
		case result := <-m.results:
			m.handleWorkflowResult(ctx, result)
		case _, ok := <-blocks:
			if !ok {
				return errors.New("withdrawal block subscription closed")
			}
			m.retryWorkflows(ctx)
		case err, ok := <-blockErrors:
			if !ok {
				return errors.New("withdrawal block error stream closed")
			}
			return err
		case <-retry.C:
			m.retryWorkflows(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Manager) recoverWithdrawals(ctx context.Context) error {
	records, err := m.cfg.Store.WorkflowStore().ListActive(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		runtime, err := m.openWorkflow(ctx, record.ID)
		if err != nil {
			return fmt.Errorf("restore withdrawal %s: %w", record.ID, err)
		}
		if err := runtime.machine.Recover(ctx); err != nil {
			runtime.needsRecovery = true
			log.Errorf("Withdrawal %s recovery deferred: %v", record.ID, err)
		}
	}
	return nil
}

func (m *Manager) openWorkflow(ctx context.Context, id string) (*withdrawalRuntime, error) {
	if runtime := m.workflows[id]; runtime != nil {
		return runtime, nil
	}
	machine, err := fsmv2.Open(ctx, m.workflowConfig(id))
	if err != nil {
		return nil, err
	}
	runtime := &withdrawalRuntime{machine: machine, monitors: make(map[string]context.CancelFunc)}
	m.workflows[id] = runtime
	return runtime, nil
}

// WithdrawDeposits retains the Go entry point; Run owns execution after admission.
func (m *Manager) WithdrawDeposits(ctx context.Context, outpoints []wire.OutPoint,
	destAddr string, satPerVbyte, amount int64) (string, string, error) {

	return m.DeliverWithdrawalRequest(ctx, outpoints, destAddr, satPerVbyte, amount)
}

func (m *Manager) startWithdrawal(ctx context.Context, request newWithdrawalRequest) error {
	if len(request.outpoints) == 0 || request.amount < 0 || request.satPerVbyte < 0 {
		return errors.New("invalid withdrawal inputs, amount or fee rate")
	}
	if err := deposit.CheckDuplicates(request.outpoints); err != nil {
		return err
	}
	if err := m.cfg.DepositManager.EnsureDepositsFresh(ctx); err != nil {
		return err
	}
	outpoints := slices.Clone(request.outpoints)
	slices.SortFunc(outpoints, func(a, b wire.OutPoint) int { return strings.Compare(a.String(), b.String()) })
	deposits, fresh := m.cfg.DepositManager.AllOutpointsActiveDeposits(outpoints, deposit.Deposited)
	if !fresh {
		var ok bool
		deposits, ok = m.cfg.DepositManager.AllOutpointsActiveDeposits(outpoints, deposit.Withdrawing)
		if !ok {
			return ErrWithdrawingMixedDeposits
		}
	}
	ids := make([]deposit.ID, len(deposits))
	for i, d := range deposits {
		if d.GetConfirmationHeight() <= 0 {
			return errors.New("unconfirmed deposits can't be withdrawn")
		}
		ids[i] = d.ID
	}
	var previous *wire.MsgTx
	var existing fsmv2.Record
	if !fresh {
		var err error
		existing, err = m.cfg.Store.WorkflowStore().FindByDeposits(ctx, ids)
		if err != nil {
			return ErrMissingPreviousWithdrawn
		}
		data, err := fsmv2.JSONCodec[WorkflowData]().Decode(existing.Data)
		if err != nil {
			return err
		}
		if existing.State != WorkflowMonitoring || len(data.Attempts) == 0 {
			return errors.New("withdrawal is already in progress or requires reconciliation")
		}
		previous, err = decodeWorkflowTx(data.Attempts[len(data.Attempts)-1].Tx)
		if err != nil {
			return err
		}
		// Preserve the approved legacy transaction's input order. Membership
		// is a set, but changing order changes the signed plan and its identity.
		ordered := make([]*deposit.Deposit, len(data.Inputs))
		planned := make([]wire.OutPoint, len(data.Inputs))
		byPoint := make(map[string]*deposit.Deposit, len(deposits))
		for _, d := range deposits {
			byPoint[d.OutPoint.String()] = d
		}
		for i, in := range data.Inputs {
			d := byPoint[in.Outpoint]
			if d == nil {
				return ErrMissingPreviousWithdrawn
			}
			ordered[i], planned[i] = d, d.OutPoint
		}
		deposits, outpoints = ordered, planned
		if request.amount == 0 && data.RequestedAmount > 0 {
			request.amount = data.RequestedAmount
		}
	}
	address, err := m.withdrawalAddress(ctx, request.destAddr, previous)
	if err != nil {
		return err
	}
	if !address.IsForNet(m.cfg.ChainParams) {
		return errors.New("withdrawal address is for another network")
	}
	var rate chainfee.SatPerKWeight
	if request.satPerVbyte == 0 {
		rate, err = m.cfg.WalletKit.EstimateFeeRate(ctx, defaultConfTarget)
		if err != nil {
			return err
		}
	} else {
		if request.satPerVbyte > math.MaxInt64/1000 {
			return errors.New("withdrawal fee rate overflow")
		}
		rate = chainfee.SatPerKVByte(request.satPerVbyte * 1000).FeePerKWeight()
	}
	weight, err := WithdrawalTxWeight(len(deposits), address,
		lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE, true)
	if err != nil {
		return err
	}
	if rate <= 0 || int64(rate) > math.MaxInt64/int64(weight) {
		return errors.New("invalid withdrawal fee rate")
	}
	params, err := m.cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return err
	}
	prevouts, err := staticutil.ToPrevOuts(deposits, params.PkScript)
	if err != nil {
		return err
	}
	tx, _, err := m.createWithdrawalTx(ctx, outpoints, deposits, prevouts,
		btcutil.Amount(request.amount), address, rate,
		lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE)
	if err != nil {
		return err
	}
	raw, err := encodeWorkflowTx(tx)
	if err != nil {
		return err
	}
	if !fresh {
		runtime, err := m.openWorkflow(ctx, existing.ID)
		if err != nil {
			return err
		}
		data, err := fsmv2.JSONCodec[WorkflowData]().Decode(existing.Data)
		if err != nil {
			return err
		}
		// A replacement must increase the absolute fee as well as the fee rate.
		var oldOutputs, newOutputs int64
		for _, out := range previous.TxOut {
			oldOutputs += out.Value
		}
		for _, out := range tx.TxOut {
			newOutputs += out.Value
		}
		if newOutputs >= oldOutputs {
			return errors.New("replacement fee must increase")
		}
		_, err = runtime.machine.Send(ctx, workflowBump, workflowEvent{
			Attempt: data.Attempt + 1, UnsignedTx: raw,
			FeeRateSatPerKWeight: int64(rate), RequestedAmount: request.amount,
		})
		if err != nil {
			runtime.needsRecovery = true
			return err
		}
		runtime.waiters = append(runtime.waiters, request.respChan)
		return nil
	}
	id, err := GetRandomWithdrawalID()
	if err != nil {
		return err
	}
	data := WorkflowData{
		ID: hex.EncodeToString(id[:]), DestinationAddress: address.String(),
		DestinationScript: bytes.Clone(tx.TxOut[0].PkScript), RequestedAmount: request.amount,
		FeeRateSatPerKWeight: int64(rate), InitiationHeight: m.initiationHeight.Load(),
		InitiationTime: time.Now().UTC().Truncate(time.Microsecond), Attempt: 1, UnsignedTx: raw,
	}
	for _, d := range deposits {
		if int64(d.Value) > math.MaxInt64-data.TotalDepositAmount {
			return errors.New("withdrawal amount overflow")
		}
		data.TotalDepositAmount += int64(d.Value)
		data.DepositIDs = append(data.DepositIDs, hex.EncodeToString(d.ID[:]))
		data.Inputs = append(data.Inputs, WorkflowInput{
			DepositID: hex.EncodeToString(d.ID[:]), Outpoint: d.OutPoint.String(),
			Value: int64(d.Value), PkScript: bytes.Clone(params.PkScript), ConfirmationHeight: d.GetConfirmationHeight(),
		})
	}
	slices.Sort(data.DepositIDs)
	machine, err := fsmv2.Create(ctx, m.workflowConfig(data.ID), WorkflowSigning, data)
	if err != nil {
		// Creation can have committed even when its acknowledgement failed.
		if runtime, loadErr := m.openWorkflow(ctx, data.ID); loadErr == nil {
			runtime.needsRecovery = true
		}
		return err
	}
	runtime := &withdrawalRuntime{machine: machine, monitors: make(map[string]context.CancelFunc),
		waiters: []chan *newWithdrawalResponse{request.respChan}}
	m.workflows[data.ID] = runtime
	if err := machine.Recover(ctx); err != nil {
		runtime.waiters = nil
		runtime.needsRecovery = true
		return err
	}
	return nil
}

// ownedWorkflowStore synchronizes durable deposit ownership with the running
// deposit FSMs before v2 invokes an effect. Its callback performs only DB work.
type ownedWorkflowStore struct {
	manager *Manager
	inner   *WorkflowStore
}

func (s *ownedWorkflowStore) Load(ctx context.Context, id string) (fsmv2.Record, error) {
	return s.inner.Load(ctx, id)
}

func (s *ownedWorkflowStore) write(ctx context.Context, record fsmv2.Record,
	write func() error) error {

	data, err := fsmv2.JSONCodec[WorkflowData]().Decode(record.Data)
	if err != nil {
		return err
	}
	outpoints, err := workflowOutpoints(data)
	if err != nil {
		return err
	}
	return s.manager.cfg.DepositManager.WithWithdrawalLock(ctx, outpoints,
		func(_ []*deposit.Deposit) error { return write() })
}

func (s *ownedWorkflowStore) Create(ctx context.Context, record fsmv2.Record) error {
	return s.write(ctx, record, func() error { return s.inner.Create(ctx, record) })
}

func (s *ownedWorkflowStore) CompareAndSwap(ctx context.Context, record fsmv2.Record,
	expected uint64) error {

	return s.write(ctx, record, func() error { return s.inner.CompareAndSwap(ctx, record, expected) })
}

func workflowOutpoints(data WorkflowData) ([]wire.OutPoint, error) {
	points := make([]wire.OutPoint, len(data.Inputs))
	for i, in := range data.Inputs {
		point, err := wire.NewOutPointFromString(in.Outpoint)
		if err != nil || point.String() != in.Outpoint {
			return nil, errors.New("invalid withdrawal outpoint encoding")
		}
		points[i] = *point
	}
	return points, nil
}

func (m *Manager) enterWorkflow(ctx context.Context, view fsmv2.Snapshot[WorkflowData]) error {
	return m.restoreWorkflow(ctx, view)
}

func (m *Manager) restoreWorkflow(ctx context.Context, view fsmv2.Snapshot[WorkflowData]) error {
	runtime := m.workflows[view.ID]
	if runtime == nil {
		return errors.New("withdrawal runtime is not registered")
	}
	outpoints, err := workflowOutpoints(view.Data)
	if err != nil {
		return err
	}
	if len(outpoints) > 0 {
		if err := m.cfg.DepositManager.ReconcileWithdrawal(ctx, outpoints); err != nil {
			return err
		}
	}
	if view.State == WorkflowSigning {
		if err := m.cfg.DepositManager.EnsureDepositsFresh(ctx); err != nil {
			return err
		}
		deposits, ok := m.cfg.DepositManager.AllOutpointsActiveDeposits(outpoints, deposit.WithdrawalReserved)
		if !ok {
			deposits, ok = m.cfg.DepositManager.AllOutpointsActiveDeposits(outpoints, deposit.Withdrawing)
		}
		if !ok {
			return errors.New("withdrawal inputs unavailable during signing recovery")
		}
		for _, d := range deposits {
			if d.GetConfirmationHeight() <= 0 {
				return errors.New("withdrawal inputs unconfirmed during signing recovery")
			}
		}
	}
	if view.State == WorkflowReconciliation {
		log.Warnf("Withdrawal %s requires reconciliation: %s", view.ID, view.Data.ReconciliationReason)
	}
	m.ensureSpendMonitors(view)
	if view.State != WorkflowConfirming && view.State != WorkflowFinalizing {
		if runtime.confirmationCancel != nil {
			runtime.confirmationCancel()
		}
		runtime.confirmationCancel = nil
		runtime.confirmationKey = ""
	}
	if view.State == WorkflowReconciliation && runtime.publishCancel != nil {
		runtime.publishCancel()
		runtime.publishCancel = nil
		runtime.publishAttempt = 0
	}
	if view.State == WorkflowSigning && runtime.signAttempt != view.Data.Attempt {
		if runtime.signCancel != nil {
			runtime.signCancel()
		}
		workCtx, cancel := context.WithCancel(m.lifecycle)
		runtime.signCancel, runtime.signAttempt = cancel, view.Data.Attempt
		m.wg.Go(func() {
			signingCtx, stop := context.WithTimeout(workCtx, time.Minute)
			defer stop()
			raw, err := m.signWorkflowPlan(signingCtx, view.Data)
			event := workflowSigned
			if err != nil {
				event = workflowAbort
			}
			m.deliverResult(workCtx, workflowResult{id: view.ID, event: event,
				message: workflowEvent{Attempt: view.Data.Attempt, Tx: raw}, err: err})
		})
	}
	if view.State != WorkflowSigning && runtime.signCancel != nil {
		runtime.signCancel()
		runtime.signCancel = nil
		runtime.signAttempt = 0
	}
	if view.State == WorkflowPublishPending || view.State == WorkflowMonitoring {
		m.ensurePublisher(view)
	}
	if view.State == WorkflowConfirming || view.State == WorkflowFinalizing {
		if runtime.publishCancel != nil {
			runtime.publishCancel()
			runtime.publishCancel = nil
			runtime.publishAttempt = 0
		}
		m.ensureConfirmation(view)
	}
	return nil
}

func (m *Manager) deliverResult(ctx context.Context, result workflowResult) {
	select {
	case m.results <- result:
	case <-ctx.Done():
	}
}

func (m *Manager) workflowAddress(data WorkflowData) string {
	if data.DestinationAddress != "" {
		return data.DestinationAddress
	}
	_, addresses, _, err := txscript.ExtractPkScriptAddrs(data.DestinationScript, m.cfg.ChainParams)
	if err == nil && len(addresses) == 1 {
		return addresses[0].String()
	}
	return ""
}

func (m *Manager) reply(runtime *withdrawalRuntime, data WorkflowData, err error) {
	txid := data.WinningTxID
	if txid == "" && len(data.Attempts) > 0 {
		txid = data.Attempts[len(data.Attempts)-1].TxID
	}
	for _, waiter := range runtime.waiters {
		waiter <- &newWithdrawalResponse{txHash: txid, withdrawalAddress: m.workflowAddress(data), err: err}
	}
	runtime.waiters = nil
}

func (m *Manager) handleWorkflowResult(ctx context.Context, result workflowResult) {
	runtime := m.workflows[result.id]
	if runtime == nil {
		return
	}
	view, err := runtime.machine.Snapshot(ctx)
	if err != nil {
		return
	}
	if result.err != nil {
		if result.event != workflowAbort {
			log.Warnf("Withdrawal %s runtime retry: %v", result.id, result.err)
			if result.event == workflowPublished && result.message.Attempt == view.Data.Attempt {
				m.reply(runtime, view.Data, result.err)
			}
			return
		}
		if result.message.Attempt != view.Data.Attempt || view.State != WorkflowSigning {
			return
		}
		if len(view.Data.Attempts) > 0 {
			result.event = workflowResume
		}
	}
	if result.event == workflowSpend && workflowCandidate(view.Data, result.message.TxID) == nil {
		result.event = workflowConflict
		result.message.Reason = "deposit spent by a transaction outside the approved withdrawal attempts"
	}
	committed, err := runtime.machine.Send(ctx, result.event, result.message)
	if err != nil {
		if errors.Is(err, fsmv2.ErrRecoveryRequired) {
			runtime.needsRecovery = true
			m.reply(runtime, view.Data, err)
		} else if !errors.Is(err, fsmv2.ErrEventRejected) {
			log.Warnf("Withdrawal %s rejected observation: %v", result.id, err)
		}
		return
	}
	switch committed.State {
	case WorkflowMonitoring:
		if result.event == workflowPublished {
			m.reply(runtime, committed.Data, nil)
		}
		if result.event == workflowResume {
			m.reply(runtime, committed.Data, result.err)
		}
	case WorkflowFailed:
		m.reply(runtime, committed.Data, result.err)
		m.stopRuntime(runtime)
		delete(m.workflows, result.id)
	case WorkflowSucceeded:
		m.reply(runtime, committed.Data, nil)
		m.stopRuntime(runtime)
		delete(m.workflows, result.id)
	case WorkflowReconciliation:
		m.reply(runtime, committed.Data, errors.New("withdrawal requires reconciliation"))
	}
}

func (m *Manager) retryWorkflows(ctx context.Context) {
	for id, runtime := range m.workflows {
		if runtime.needsRecovery {
			// A signer canceled by a failed commit must be allowed a fresh session.
			if runtime.signCancel != nil {
				runtime.signCancel()
				runtime.signCancel = nil
			}
			runtime.signAttempt = 0
			if runtime.confirmationCancel != nil {
				runtime.confirmationCancel()
			}
			runtime.confirmationCancel = nil
			runtime.confirmationKey = ""
			if err := runtime.machine.Recover(ctx); err != nil {
				log.Warnf("Withdrawal %s recovery retry: %v", id, err)
				continue
			}
			runtime.needsRecovery = false
			view, err := runtime.machine.Snapshot(ctx)
			if err == nil && (view.State == WorkflowSucceeded || view.State == WorkflowFailed) {
				points, parseErr := workflowOutpoints(view.Data)
				if parseErr == nil && len(points) > 0 {
					if err := m.cfg.DepositManager.ReconcileWithdrawal(ctx, points); err != nil {
						runtime.needsRecovery = true
						m.stopRuntime(runtime)
						continue
					}
				}
				m.reply(runtime, view.Data, nil)
				m.stopRuntime(runtime)
				delete(m.workflows, id)
			}
		}
		if runtime.publishWake != nil {
			select {
			case runtime.publishWake <- struct{}{}:
			default:
			}
		}
	}
}

func (m *Manager) stopRuntime(runtime *withdrawalRuntime) {
	if runtime.signCancel != nil {
		runtime.signCancel()
		runtime.signCancel = nil
	}
	if runtime.publishCancel != nil {
		runtime.publishCancel()
		runtime.publishCancel = nil
	}
	if runtime.confirmationCancel != nil {
		runtime.confirmationCancel()
		runtime.confirmationCancel = nil
	}
	for key, cancel := range runtime.monitors {
		cancel()
		delete(runtime.monitors, key)
	}
}

// DeliverWithdrawalRequest preserves RPC request/reply behavior. A canceled
// caller cannot block the owner or cancel an already durable obligation.
func (m *Manager) DeliverWithdrawalRequest(ctx context.Context, outpoints []wire.OutPoint,
	destAddr string, satPerVbyte, amount int64) (string, string, error) {

	request := newWithdrawalRequest{outpoints: slices.Clone(outpoints), destAddr: destAddr,
		satPerVbyte: satPerVbyte, amount: amount, respChan: make(chan *newWithdrawalResponse, 1)}
	select {
	case m.newWithdrawalRequestChan <- request:
	case <-m.exitChan:
		return "", "", errors.New("withdrawal manager stopped")
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	select {
	case result := <-request.respChan:
		return result.txHash, result.withdrawalAddress, result.err
	case <-m.exitChan:
		return "", "", errors.New("withdrawal manager stopped")
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

// GetAllWithdrawals lists the new authoritative withdrawal schema only.
func (m *Manager) GetAllWithdrawals(ctx context.Context) ([]Withdrawal, error) {
	return m.cfg.Store.GetAllWithdrawals(ctx)
}
