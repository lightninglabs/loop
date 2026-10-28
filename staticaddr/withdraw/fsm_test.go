package withdraw

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	fsmv2 "github.com/lightninglabs/loop/fsm/v2"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

// signTestCandidate uses real Taproot signatures so guard tests distinguish an
// authorized plan from a different, independently valid signed transaction.
func signTestCandidate(t *testing.T, fixture *persistenceFixture,
	data WorkflowData, tx *wire.MsgTx) []byte {

	t.Helper()
	prevouts := make(map[wire.OutPoint]*wire.TxOut, len(data.Inputs))
	for i, in := range data.Inputs {
		prevouts[tx.TxIn[i].PreviousOutPoint] = &wire.TxOut{
			Value: in.Value, PkScript: in.PkScript,
		}
	}
	fetcher := txscript.NewMultiPrevOutFetcher(prevouts)
	hashes := txscript.NewTxSigHashes(tx, fetcher)
	for i, in := range tx.TxIn {
		prev := prevouts[in.PreviousOutPoint]
		sig, err := txscript.RawTxInTaprootSignature(tx, hashes, i,
			prev.Value, prev.PkScript, nil, txscript.SigHashDefault, fixture.key)
		require.NoError(t, err)
		in.Witness = wire.TxWitness{sig}
	}
	raw, err := encodeWorkflowTx(tx)
	require.NoError(t, err)
	return raw
}

func TestWorkflowSignedCandidateSecurity(t *testing.T) {
	fixture := newPersistenceFixture(t, 2)
	data, signed := fixture.intent(t, fixture.inputs, 91)
	raw, err := encodeWorkflowTx(signed)
	require.NoError(t, err)
	view := fsmv2.Snapshot[WorkflowData]{Data: data, State: WorkflowSigning}
	gate := workflowGate(workflowSigned)
	require.NoError(t, gate(t.Context(), view, workflowEvent{Attempt: 1, Tx: raw}))
	require.ErrorContains(t, gate(t.Context(), view, workflowEvent{Attempt: 2, Tx: raw}), "stale")

	// Valid signatures alone are insufficient: changing the approved amount
	// still requires rejection even when the destination remains unchanged.
	different := signed.Copy()
	different.TxOut[0].Value--
	differentRaw := signTestCandidate(t, fixture, data, different)
	_, err = validateWorkflowCandidate(data, differentRaw, true)
	require.NoError(t, err)
	require.ErrorContains(t, gate(t.Context(), view,
		workflowEvent{Attempt: 1, Tx: differentRaw}), "approved plan")

	broken := signed.Copy()
	broken.TxIn[0].Witness[0][0] ^= 1
	brokenRaw, err := encodeWorkflowTx(broken)
	require.NoError(t, err)
	require.Error(t, gate(t.Context(), view, workflowEvent{Attempt: 1, Tx: brokenRaw}))
	require.Error(t, gate(t.Context(), view,
		workflowEvent{Attempt: 1, Tx: append(bytes.Clone(raw), 0)}))

	subset := signed.Copy()
	subset.TxIn = subset.TxIn[:1]
	subsetRaw, err := encodeWorkflowTx(subset)
	require.NoError(t, err)
	require.Error(t, gate(t.Context(), view, workflowEvent{Attempt: 1, Tx: subsetRaw}))

	change := signed.Copy()
	change.TxOut[0].Value -= 100
	change.AddTxOut(&wire.TxOut{Value: 100, PkScript: []byte{txscript.OP_TRUE}})
	changeRaw := signTestCandidate(t, fixture, data, change)
	require.ErrorContains(t, gate(t.Context(), view,
		workflowEvent{Attempt: 1, Tx: changeRaw}), "change is not owned")
}

// graphConfig exercises the real workflow graph and domain transactions while
// controlling asynchronous work independently in the dedicated worker tests.
func graphConfig(fixture *persistenceFixture, data WorkflowData) fsmv2.Config[WorkflowData, workflowEvent] {
	manager := &Manager{cfg: &ManagerConfig{Store: fixture.store}}
	cfg := manager.workflowConfig(data.ID)
	cfg.Store = fixture.store.WorkflowStore()
	for id, state := range cfg.States {
		if !state.Terminal {
			state.Enter = func(context.Context, fsmv2.Snapshot[WorkflowData]) error { return nil }
			state.Restore = func(context.Context, fsmv2.Snapshot[WorkflowData]) error { return nil }
			cfg.States[id] = state
		}
	}
	return cfg
}

func TestWorkflowOldCandidateWinsDuringReplacementSigning(t *testing.T) {
	fixture := newPersistenceFixture(t, 2)
	data, signed := fixture.intent(t, fixture.inputs, 92)
	cfg := graphConfig(fixture, data)
	machine, err := fsmv2.Create(t.Context(), cfg, WorkflowSigning, data)
	require.NoError(t, err)
	require.NoError(t, machine.Recover(t.Context()))
	raw, err := encodeWorkflowTx(signed)
	require.NoError(t, err)
	_, err = machine.Send(t.Context(), workflowSigned, workflowEvent{Attempt: 1, Tx: raw})
	require.NoError(t, err)
	_, err = machine.Send(t.Context(), workflowPublished,
		workflowEvent{Attempt: 1, TxID: signed.TxHash().String()})
	require.NoError(t, err)
	replacement, err := decodeWorkflowTx(data.UnsignedTx)
	require.NoError(t, err)
	replacement.TxOut[0].Value -= 1000
	replacementRaw, err := encodeWorkflowTx(replacement)
	require.NoError(t, err)
	bumping, err := machine.Send(t.Context(), workflowBump, workflowEvent{
		Attempt: 2, UnsignedTx: replacementRaw, FeeRateSatPerKWeight: 2000,
	})
	require.NoError(t, err)
	require.Equal(t, WorkflowSigning, bumping.State)
	require.Len(t, bumping.Data.Attempts, 1)

	// The old signer result is stale, but a chain observation of its already
	// retained candidate must remain actionable during the new attempt.
	_, err = machine.Send(t.Context(), workflowSigned, workflowEvent{Attempt: 1, Tx: raw})
	require.ErrorContains(t, err, "stale")
	confirming, err := machine.Send(t.Context(), workflowSpend,
		workflowEvent{TxID: signed.TxHash().String(), Tx: raw})
	require.NoError(t, err)
	require.Equal(t, WorkflowConfirming, confirming.State)
	require.Equal(t, signed.TxHash().String(), confirming.Data.WinningTxID)
	lateReplacement := signTestCandidate(t, fixture, data, replacement)
	_, err = machine.Send(t.Context(), workflowSigned,
		workflowEvent{Attempt: 2, Tx: lateReplacement})
	require.ErrorIs(t, err, fsmv2.ErrEventRejected)
	winner, err := machine.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, winner.Data.Attempts, 1)
	require.Equal(t, signed.TxHash().String(), winner.Data.WinningTxID)
}

func TestWorkflowFinalizationBindsConfirmationAnchor(t *testing.T) {
	fixture := newPersistenceFixture(t, 1)
	data, signed := fixture.intent(t, fixture.inputs, 93)
	raw, err := encodeWorkflowTx(signed)
	require.NoError(t, err)
	data.Attempts = []WorkflowAttempt{{ID: 1, Tx: raw, TxID: signed.TxHash().String()}}
	data.WinningTxID = signed.TxHash().String()
	data.ConfirmationHeight = 120
	data.ConfirmationBlockHash = chainhash.Hash{1}.String()
	data.WithdrawnAmount = signed.TxOut[0].Value
	view := fsmv2.Snapshot[WorkflowData]{Data: data, State: WorkflowFinalizing}
	gate := workflowGate(workflowFinalize)
	valid := workflowEvent{TxID: data.WinningTxID, Height: data.ConfirmationHeight,
		BlockHash: data.ConfirmationBlockHash}
	require.NoError(t, gate(t.Context(), view, valid))
	for name, mutate := range map[string]func(*workflowEvent){
		"stale tx":         func(event *workflowEvent) { event.TxID = chainhash.Hash{2}.String() },
		"stale height":     func(event *workflowEvent) { event.Height-- },
		"reorged block":    func(event *workflowEvent) { event.BlockHash = chainhash.Hash{3}.String() },
		"missing evidence": func(event *workflowEvent) { *event = workflowEvent{} },
	} {
		t.Run(name, func(t *testing.T) {
			message := valid
			mutate(&message)
			require.Error(t, gate(t.Context(), view, message))
		})
	}
	view.State = WorkflowConfirming
	require.Error(t, workflowGate(workflowConfirmed)(t.Context(), view,
		workflowEvent{TxID: data.WinningTxID, Height: 120,
			BlockHash: strings.Repeat("z", chainhash.MaxHashStringSize)}))
}

type ownershipCommitFailure struct {
	DepositManager

	after bool
	err   error
}

func (m *ownershipCommitFailure) WithWithdrawalLock(_ context.Context,
	_ []wire.OutPoint, update func([]*deposit.Deposit) error) error {

	if m.after {
		if err := update(nil); err != nil {
			return err
		}
	}
	return m.err
}

func TestWorkflowOwnershipWriteErrorPreventsPublication(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before commit", true: "after commit"}[committed], func(t *testing.T) {
			fixture := newPersistenceFixture(t, 1)
			data, signed := fixture.intent(t, fixture.inputs, 94)
			initial := persistenceRecord(t, data, WorkflowSigning, 1)
			require.NoError(t, fixture.store.WorkflowStore().Create(t.Context(), initial))
			writeErr := errors.New("ownership transaction outcome unknown")
			manager := &Manager{cfg: &ManagerConfig{
				Store: fixture.store, DepositManager: &ownershipCommitFailure{
					after: committed, err: writeErr,
				},
			}}
			cfg := graphConfig(fixture, data)
			cfg.Store = &ownedWorkflowStore{manager: manager, inner: fixture.store.WorkflowStore()}
			published := 0
			pending := cfg.States[WorkflowPublishPending]
			pending.Enter = func(context.Context, fsmv2.Snapshot[WorkflowData]) error {
				published++
				return nil
			}
			cfg.States[WorkflowPublishPending] = pending
			machine, err := fsmv2.Open(t.Context(), cfg)
			require.NoError(t, err)
			require.NoError(t, machine.Recover(t.Context()))
			raw, err := encodeWorkflowTx(signed)
			require.NoError(t, err)
			view, err := machine.Send(t.Context(), workflowSigned,
				workflowEvent{Attempt: 1, Tx: raw})
			require.ErrorIs(t, err, writeErr)
			require.ErrorIs(t, err, fsmv2.ErrRecoveryRequired)
			require.Zero(t, view.Revision)
			require.Zero(t, published)
			durable, err := fixture.store.WorkflowStore().Load(t.Context(), data.ID)
			require.NoError(t, err)
			if committed {
				require.Equal(t, WorkflowPublishPending, durable.State)
			} else {
				require.Equal(t, WorkflowSigning, durable.State)
			}
			_, err = machine.Send(t.Context(), workflowSigned,
				workflowEvent{Attempt: 1, Tx: raw})
			require.ErrorIs(t, err, fsmv2.ErrRecoveryRequired)
		})
	}
}

func TestWorkflowCanceledReplyDoesNotBlockOwner(t *testing.T) {
	manager, err := NewManager(&ManagerConfig{}, 1)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	caller := make(chan error, 1)
	go func() {
		_, _, err := manager.DeliverWithdrawalRequest(ctx, nil, "", 0, 0)
		caller <- err
	}()
	request := <-manager.newWithdrawalRequestChan
	cancel()
	require.ErrorIs(t, <-caller, context.Canceled)
	owner := make(chan struct{})
	go func() {
		runtime := &withdrawalRuntime{waiters: []chan *newWithdrawalResponse{request.respChan}}
		manager.reply(runtime, WorkflowData{}, errors.New("completed after caller left"))
		close(owner)
	}()
	select {
	case <-owner:
	case <-time.After(time.Second):
		t.Fatal("abandoned reply blocked withdrawal owner")
	}
}

type workflowTestNotifier struct {
	lndclient.ChainNotifierClient

	mu            sync.Mutex
	spendCalls    chan wire.OutPoint
	closeFirst    bool
	spendCount    int
	confirmations chan *chainntnfs.TxConfirmation
	failures      chan error
}

func (n *workflowTestNotifier) RegisterSpendNtfn(ctx context.Context,
	outpoint *wire.OutPoint, _ []byte, _ int32,
	_ ...lndclient.NotifierOption) (chan *chainntnfs.SpendDetail, chan error, error) {

	n.mu.Lock()
	n.spendCount++
	closeStream := n.closeFirst && n.spendCount == 1
	n.mu.Unlock()
	select {
	case n.spendCalls <- *outpoint:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	spends := make(chan *chainntnfs.SpendDetail)
	if closeStream {
		close(spends)
	}
	return spends, make(chan error), nil
}

func (n *workflowTestNotifier) RegisterConfirmationsNtfn(context.Context,
	*chainhash.Hash, []byte, int32, int32,
	...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation, chan error, error) {

	return n.confirmations, n.failures, nil
}

func TestWorkflowSpendMonitorRetriesClosedStream(t *testing.T) {
	fixture := newPersistenceFixture(t, 1)
	data, _ := fixture.intent(t, fixture.inputs, 95)
	notifier := &workflowTestNotifier{spendCalls: make(chan wire.OutPoint, 2), closeFirst: true}
	manager, err := NewManager(&ManagerConfig{ChainNotifier: notifier}, 100)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	manager.lifecycle = ctx
	manager.workflows[data.ID] = &withdrawalRuntime{monitors: make(map[string]context.CancelFunc)}
	manager.ensureSpendMonitors(fsmv2.Snapshot[WorkflowData]{ID: data.ID, Data: data})
	for range 2 {
		select {
		case point := <-notifier.spendCalls:
			require.Equal(t, fixture.inputs[0].OutPoint, point)
		case <-time.After(7 * time.Second):
			t.Fatal("closed spend subscription was not retried")
		}
	}
	cancel()
	manager.wg.Wait()
}

func TestWorkflowConfirmationDetectsReorgBeforeFinalization(t *testing.T) {
	fixture := newPersistenceFixture(t, 1)
	data, signed := fixture.intent(t, fixture.inputs, 96)
	data.WinningTxID = signed.TxHash().String()
	data.ConfirmationHeight = 120
	data.ConfirmationBlockHash = chainhash.Hash{1}.String()
	notifier := &workflowTestNotifier{
		confirmations: make(chan *chainntnfs.TxConfirmation, 1), failures: make(chan error),
	}
	manager, err := NewManager(&ManagerConfig{ChainNotifier: notifier}, 100)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	manager.lifecycle = ctx
	manager.workflows[data.ID] = &withdrawalRuntime{monitors: make(map[string]context.CancelFunc)}
	manager.ensureConfirmation(fsmv2.Snapshot[WorkflowData]{ID: data.ID,
		State: WorkflowFinalizing, Data: data})
	replacementBlock := chainhash.Hash{2}
	notifier.confirmations <- &chainntnfs.TxConfirmation{
		Tx: signed, BlockHeight: 121, BlockHash: &replacementBlock,
	}
	select {
	case result := <-manager.results:
		require.Equal(t, workflowReorg, result.event)
		require.Equal(t, data.WinningTxID, result.message.TxID)
	case <-time.After(time.Second):
		t.Fatal("confirmation reorg was not delivered to workflow owner")
	}
	cancel()
	manager.wg.Wait()
}

type workflowRuntimeDeposits struct{ DepositManager }

func (*workflowRuntimeDeposits) ReconcileWithdrawal(context.Context, []wire.OutPoint) error {
	return nil
}

// TestWorkflowRecoveryReattachesEndedConfirmation covers a worker that delivered
// its result before a later uncertain write forced recovery of the same durable
// phase. An ended worker must not suppress its replacement merely because the
// phase and confirmation anchor are unchanged.
func TestWorkflowRecoveryReattachesEndedConfirmation(t *testing.T) {
	fixture := newPersistenceFixture(t, 1)
	data, signed := fixture.intent(t, fixture.inputs, 97)
	store := fixture.store.WorkflowStore()
	require.NoError(t, store.Create(t.Context(), persistenceRecord(t, data, WorkflowSigning, 1)))
	raw, err := encodeWorkflowTx(signed)
	require.NoError(t, err)
	data.Attempts = []WorkflowAttempt{{ID: 1, Tx: raw, TxID: signed.TxHash().String()}}
	data.WinningTxID = signed.TxHash().String()
	data.ConfirmationHeight = 120
	block := chainhash.Hash{4}
	data.ConfirmationBlockHash = block.String()
	data.WithdrawnAmount = signed.TxOut[0].Value
	require.NoError(t, store.CompareAndSwap(t.Context(),
		persistenceRecord(t, data, WorkflowFinalizing, 2), 1))
	notifier := &workflowTestNotifier{spendCalls: make(chan wire.OutPoint, 2),
		confirmations: make(chan *chainntnfs.TxConfirmation, 1), failures: make(chan error)}
	manager, err := NewManager(&ManagerConfig{Store: fixture.store,
		ChainNotifier: notifier, DepositManager: &workflowRuntimeDeposits{}}, 100)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); manager.wg.Wait() }()
	manager.lifecycle = ctx
	runtime, err := manager.openWorkflow(ctx, data.ID)
	require.NoError(t, err)
	require.NoError(t, runtime.machine.Recover(ctx))

	for i := range 2 {
		if i != 0 {
			runtime.needsRecovery = true
			manager.retryWorkflows(ctx)
			require.False(t, runtime.needsRecovery)
		}
		notifier.confirmations <- &chainntnfs.TxConfirmation{
			Tx: signed, BlockHeight: 120, BlockHash: &block,
		}
		select {
		case result := <-manager.results:
			require.Equal(t, workflowFinalize, result.event)
			require.Equal(t, block.String(), result.message.BlockHash)
		case <-time.After(time.Second):
			t.Fatalf("confirmation worker was not restored on pass %d", i)
		}
	}
}

func TestWorkflowFailedReplacementPreservesApprovedCandidate(t *testing.T) {
	fixture := newPersistenceFixture(t, 1)
	data, signed := fixture.intent(t, fixture.inputs, 98)
	machine, err := fsmv2.Create(t.Context(), graphConfig(fixture, data), WorkflowSigning, data)
	require.NoError(t, err)
	require.NoError(t, machine.Recover(t.Context()))
	raw, err := encodeWorkflowTx(signed)
	require.NoError(t, err)
	_, err = machine.Send(t.Context(), workflowSigned, workflowEvent{Attempt: 1, Tx: raw})
	require.NoError(t, err)
	_, err = machine.Send(t.Context(), workflowPublished,
		workflowEvent{Attempt: 1, TxID: signed.TxHash().String()})
	require.NoError(t, err)
	replacement, err := decodeWorkflowTx(data.UnsignedTx)
	require.NoError(t, err)
	replacement.TxOut[0].Value -= 1000
	plan, err := encodeWorkflowTx(replacement)
	require.NoError(t, err)
	_, err = machine.Send(t.Context(), workflowBump, workflowEvent{
		Attempt: 2, UnsignedTx: plan, FeeRateSatPerKWeight: 2000,
	})
	require.NoError(t, err)
	resumed, err := machine.Send(t.Context(), workflowResume, workflowEvent{Attempt: 2})
	require.NoError(t, err)
	require.Equal(t, WorkflowMonitoring, resumed.State)
	require.Len(t, resumed.Data.Attempts, 1)
	require.Equal(t, raw, resumed.Data.Attempts[0].Tx)
	require.Equal(t, data.FeeRateSatPerKWeight, resumed.Data.FeeRateSatPerKWeight)
	for _, input := range fixture.inputs {
		stored, err := fixture.deposits.GetDeposit(t.Context(), input.ID)
		require.NoError(t, err)
		require.Equal(t, deposit.Withdrawing, stored.GetState())
	}
	// A failed, never-published attempt must not raise the minimum approved
	// rate for the next request above the candidate still being monitored.
	_, err = machine.Send(t.Context(), workflowBump, workflowEvent{
		Attempt: 3, UnsignedTx: plan, FeeRateSatPerKWeight: 1500,
	})
	require.NoError(t, err)
}

func TestWorkflowPublicationResultBindsAttemptCandidate(t *testing.T) {
	fixture := newPersistenceFixture(t, 1)
	data, first := fixture.intent(t, fixture.inputs, 99)
	firstRaw, err := encodeWorkflowTx(first)
	require.NoError(t, err)
	second := first.Copy()
	second.TxOut[0].Value--
	secondRaw := signTestCandidate(t, fixture, data, second)
	data.Attempt = 2
	data.Attempts = []WorkflowAttempt{
		{ID: 1, Tx: firstRaw, TxID: first.TxHash().String()},
		{ID: 2, Tx: secondRaw, TxID: second.TxHash().String()},
	}
	view := fsmv2.Snapshot[WorkflowData]{State: WorkflowPublishPending, Data: data}
	gate := workflowGate(workflowPublished)
	require.NoError(t, gate(t.Context(), view,
		workflowEvent{Attempt: 2, TxID: second.TxHash().String()}))
	require.Error(t, gate(t.Context(), view,
		workflowEvent{Attempt: 2, TxID: first.TxHash().String()}))
	require.Error(t, gate(t.Context(), view,
		workflowEvent{Attempt: 1, TxID: first.TxHash().String()}))
}

func TestWorkflowRestorationRejectsImpossibleAccounting(t *testing.T) {
	fixture := newPersistenceFixture(t, 1)
	data, signed := fixture.intent(t, fixture.inputs, 100)
	raw, err := encodeWorkflowTx(signed)
	require.NoError(t, err)
	data.Attempts = []WorkflowAttempt{{ID: 1, Tx: raw, TxID: signed.TxHash().String(),
		FeeRateSatPerKWeight: data.FeeRateSatPerKWeight}}
	data.WinningTxID = signed.TxHash().String()
	data.ConfirmationHeight = 120
	data.ConfirmationBlockHash = chainhash.Hash{1}.String()
	data.WithdrawnAmount = signed.TxOut[0].Value
	for _, state := range []fsmv2.StateID{WorkflowFinalizing, WorkflowSucceeded} {
		require.NoError(t, validateWorkflow(state, data))
		for name, mutate := range map[string]func(*WorkflowData){
			"recipient mismatch": func(changed *WorkflowData) { changed.WithdrawnAmount-- },
			"invented change":    func(changed *WorkflowData) { changed.ChangeAmount = 1 },
			"overspend":          func(changed *WorkflowData) { changed.WithdrawnAmount = changed.TotalDepositAmount + 1 },
		} {
			t.Run(string(state)+"/"+name, func(t *testing.T) {
				changed := data
				mutate(&changed)
				require.Error(t, validateWorkflow(state, changed))
			})
		}
	}

	// Historical summaries without raw bytes still need basic bounded
	// accounting and agreement with their known membership before deposits
	// can be considered completed during import.
	data.LegacySource = "legacy-withdrawal:accounting-test"
	data.Attempts = nil
	require.NoError(t, validateWorkflow(WorkflowSucceeded, data))
	for name, mutate := range map[string]func(*WorkflowData){
		"wrong member total": func(changed *WorkflowData) { changed.TotalDepositAmount++ },
		"recipient exceeds inputs": func(changed *WorkflowData) {
			changed.WithdrawnAmount = changed.TotalDepositAmount + 1
		},
		"change exceeds balance": func(changed *WorkflowData) { changed.ChangeAmount = changed.TotalDepositAmount },
		"missing member facts":   func(changed *WorkflowData) { changed.Inputs = nil },
	} {
		t.Run("historical/"+name, func(t *testing.T) {
			changed := data
			mutate(&changed)
			require.Error(t, validateWorkflow(WorkflowSucceeded, changed))
		})
	}
}

type workflowPlanningDeposits struct {
	DepositManager

	inputs map[wire.OutPoint]*deposit.Deposit
}

func (*workflowPlanningDeposits) EnsureDepositsFresh(context.Context) error { return nil }

func (p *workflowPlanningDeposits) AllOutpointsActiveDeposits(outpoints []wire.OutPoint,
	state fsm.StateType) ([]*deposit.Deposit, bool) {

	var selected []*deposit.Deposit
	for _, outpoint := range outpoints {
		input := p.inputs[outpoint]
		if input == nil || input.GetState() != state {
			return nil, false
		}
		selected = append(selected, input)
	}
	return selected, true
}

type workflowPlanningAddress struct {
	AddressManager

	params  *script.Parameters
	address *script.StaticAddress
}

func (a *workflowPlanningAddress) GetStaticAddressParameters(context.Context) (*script.Parameters, error) {
	return a.params, nil
}

func (a *workflowPlanningAddress) GetStaticAddress(context.Context) (*script.StaticAddress, error) {
	return a.address, nil
}

func TestWorkflowImportedPartialReplacementPreservesIntent(t *testing.T) {
	fixture := newPersistenceFixture(t, 2)
	// Legacy clients retained the caller's input order, which can differ from
	// the new manager's canonical membership selection order.
	inputs := []*deposit.Deposit{fixture.inputs[1], fixture.inputs[0]}
	data, signed := fixture.intent(t, inputs, 101)
	signed.TxOut[0].Value = 150_000
	signed.AddTxOut(&wire.TxOut{Value: data.TotalDepositAmount - 151_000,
		PkScript: fixture.script})
	raw := signTestCandidate(t, fixture, data, signed)
	legacyTx, err := decodeWorkflowTx(raw)
	require.NoError(t, err)
	require.NoError(t, fixture.store.CreateWithdrawal(t.Context(), inputs))
	fixture.saveLegacyDeposits(t, inputs, legacyTx)
	require.NoError(t, fixture.store.WorkflowStore().MigrateLegacy(t.Context()))
	records, err := fixture.store.WorkflowStore().ListActive(t.Context())
	require.NoError(t, err)
	require.Len(t, records, 1)
	imported, err := storeDecodeWorkflow(records[0])
	require.NoError(t, err)
	require.Equal(t, WorkflowMonitoring, records[0].State)
	require.Equal(t, int64(150_000), imported.RequestedAmount)
	cfg := graphConfig(fixture, imported)
	machine, err := fsmv2.Open(t.Context(), cfg)
	require.NoError(t, err)
	require.NoError(t, machine.Recover(t.Context()))
	selected := &workflowPlanningDeposits{inputs: make(map[wire.OutPoint]*deposit.Deposit)}
	for _, input := range inputs {
		selected.inputs[input.OutPoint] = input
	}
	manager, err := NewManager(&ManagerConfig{
		Store: fixture.store, DepositManager: selected, ChainParams: &chaincfg.RegressionNetParams,
		AddressManager: &workflowPlanningAddress{
			params:  &script.Parameters{PkScript: fixture.script},
			address: &script.StaticAddress{TaprootKey: txscript.ComputeTaprootKeyNoScript(fixture.key.PubKey())},
		},
	}, 100)
	require.NoError(t, err)
	manager.workflows[imported.ID] = &withdrawalRuntime{machine: machine,
		monitors: make(map[string]context.CancelFunc)}
	require.NoError(t, manager.startWithdrawal(t.Context(), newWithdrawalRequest{
		outpoints:   []wire.OutPoint{inputs[1].OutPoint, inputs[0].OutPoint},
		satPerVbyte: 10, amount: 0, respChan: make(chan *newWithdrawalResponse, 1),
	}))
	bumping, err := machine.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, WorkflowSigning, bumping.State)
	plan, err := decodeWorkflowTx(bumping.Data.UnsignedTx)
	require.NoError(t, err)
	require.Equal(t, legacyTx.TxIn[0].PreviousOutPoint, plan.TxIn[0].PreviousOutPoint)
	require.Equal(t, legacyTx.TxIn[1].PreviousOutPoint, plan.TxIn[1].PreviousOutPoint)
	require.Equal(t, int64(150_000), plan.TxOut[0].Value)
	require.Equal(t, legacyTx.TxOut[0].PkScript, plan.TxOut[0].PkScript)
	require.Len(t, plan.TxOut, 2)
	require.Equal(t, fixture.script, plan.TxOut[1].PkScript)
	require.Positive(t, plan.TxOut[1].Value)
	require.Less(t, plan.TxOut[1].Value, legacyTx.TxOut[1].Value)
	require.Len(t, bumping.Data.Attempts, 1)
	require.Equal(t, raw, bumping.Data.Attempts[0].Tx)
}
