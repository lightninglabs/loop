package fsm_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	fsm "github.com/lightninglabs/loop/fsm/v2"
)

var errInjected = errors.New("injected failure")

const (
	stateReady       = "ready"
	statePending     = "pending"
	stateDone        = "done"
	paymentOperation = "swap-1/payment"
	phaseGate        = "gate"
	phaseEnter       = "enter"
	phaseUpdate      = "update"
	phaseRestore     = "restore"
)

// memoryStore models the required atomic register, including the ambiguous
// outcome where a durable commit succeeds but its acknowledgement is lost.
type memoryStore struct {
	mu            sync.Mutex
	record        *fsm.Record
	writes        int
	failWrite     bool
	commitOnError bool
	loadErr       error
}

func copyRecord(record fsm.Record) fsm.Record {
	record.Data = bytes.Clone(record.Data)
	return record
}

func (s *memoryStore) Create(_ context.Context, record fsm.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record != nil {
		return fsm.ErrConflict
	}
	if record.Revision != 1 {
		return fmt.Errorf("initial revision: %d", record.Revision)
	}
	s.writes++
	if !s.failWrite || s.commitOnError {
		cloned := copyRecord(record)
		s.record = &cloned
	}
	if s.failWrite {
		return errInjected
	}
	return nil
}

func (s *memoryStore) Load(_ context.Context, id string) (fsm.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return fsm.Record{}, s.loadErr
	}
	if s.record == nil || s.record.ID != id {
		return fsm.Record{}, fsm.ErrNotFound
	}
	return copyRecord(*s.record), nil
}

func (s *memoryStore) CompareAndSwap(_ context.Context, next fsm.Record,
	expected uint64) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record == nil || s.record.ID != next.ID ||
		s.record.Revision != expected {

		return fsm.ErrConflict
	}
	if next.Revision != expected+1 {
		return fmt.Errorf("invalid next revision: %d", next.Revision)
	}
	s.writes++
	if !s.failWrite || s.commitOnError {
		cloned := copyRecord(next)
		s.record = &cloned
	}
	if s.failWrite {
		return errInjected
	}
	return nil
}

func (s *memoryStore) fail(afterCommit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWrite = true
	s.commitOnError = afterCommit
}

func (s *memoryStore) repair() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWrite = false
	s.loadErr = nil
}

func (s *memoryStore) rewrite(update func(*fsm.Record)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update(s.record)
}

type businessData struct {
	Values    []int
	Operation string
}

type command struct {
	Values []int
}

func allow(context.Context, fsm.Snapshot[businessData], command) error {
	return nil
}

func unchanged(_ context.Context, snapshot fsm.Snapshot[businessData],
	_ command) (businessData, error) {

	return snapshot.Data, nil
}

func noop(context.Context, fsm.Snapshot[businessData]) error {
	return nil
}

func config(store *memoryStore) fsm.Config[businessData, command] {
	validate := func(data businessData) error {
		if len(data.Values) == 0 || data.Values[0] < 0 {
			return errors.New("invalid business data")
		}
		return nil
	}
	return fsm.Config[businessData, command]{
		ID:         "swap-1",
		Version:    1,
		Store:      store,
		DataCodec:  fsm.JSONCodec[businessData](),
		EventCodec: fsm.JSONCodec[command](),
		States: map[fsm.StateID]fsm.State[businessData, command]{
			stateReady: {
				Validate: validate,
				Restore:  noop,
				Transitions: map[fsm.Event]fsm.Transition[businessData, command]{
					"start": {
						To:   statePending,
						Gate: allow,
						Update: func(_ context.Context,
							snapshot fsm.Snapshot[businessData],
							payload command) (businessData, error) {

							snapshot.Data.Values = append(
								snapshot.Data.Values,
								payload.Values...,
							)
							snapshot.Data.Operation = paymentOperation
							return snapshot.Data, nil
						},
					},
				},
			},
			statePending: {
				Validate: validate,
				Restore:  noop,
				Enter:    noop,
				Transitions: map[fsm.Event]fsm.Transition[businessData, command]{
					"complete": {
						To:     stateDone,
						Gate:   allow,
						Update: unchanged,
					},
				},
			},
			stateDone: {Terminal: true, Validate: validate},
		},
	}
}

func create(t *testing.T, cfg fsm.Config[businessData, command]) *fsm.Machine[businessData, command] {
	t.Helper()
	machine, err := fsm.Create(t.Context(), cfg, stateReady, businessData{
		Values: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	return machine
}

func snapshot(t *testing.T, machine *fsm.Machine[businessData, command]) fsm.Snapshot[businessData] {
	t.Helper()
	view, err := machine.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func assertError(t *testing.T, err, expected error) {
	t.Helper()
	if !errors.Is(err, expected) {
		t.Fatalf("expected error %v, got %v", expected, err)
	}
}

func TestExplicitRecoveryAndTerminal(t *testing.T) {
	store := &memoryStore{}
	cfg := config(store)
	restores := 0
	ready := cfg.States[stateReady]
	ready.Restore = func(context.Context, fsm.Snapshot[businessData]) error {
		restores++
		return nil
	}
	cfg.States[stateReady] = ready
	machine, err := fsm.Create(t.Context(), cfg, stateReady, businessData{
		Values: []int{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if restores != 0 {
		t.Fatal("Create ran recovery callback")
	}
	_, err = machine.Send(t.Context(), "start", command{})
	assertError(t, err, fsm.ErrRecoveryRequired)
	if err := machine.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if restores != 1 {
		t.Fatalf("expected one restore, got %d", restores)
	}
	if _, err := machine.Send(t.Context(), "start", command{}); err != nil {
		t.Fatal(err)
	}
	completed, err := machine.Send(t.Context(), "complete", command{})
	if err != nil || completed.State != stateDone || completed.Revision != 3 {
		t.Fatalf("unexpected completion: %+v, %v", completed, err)
	}
	_, err = machine.Send(t.Context(), "start", command{})
	assertError(t, err, fsm.ErrEventRejected)

	reopened, err := fsm.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reopened.Send(t.Context(), "start", command{})
	assertError(t, err, fsm.ErrRecoveryRequired)
	if err := reopened.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = reopened.Send(t.Context(), "start", command{})
	assertError(t, err, fsm.ErrEventRejected)
	if restores != 1 || store.writes != 3 {
		t.Fatalf("terminal recovery replayed work: restores=%d writes=%d", restores, store.writes)
	}
}

func TestRejectedUpdatesAreIsolated(t *testing.T) {
	for _, failure := range []string{phaseGate, phaseUpdate, "validate"} {
		t.Run(failure, func(t *testing.T) {
			store := &memoryStore{}
			cfg := config(store)
			ready := cfg.States[stateReady]
			transition := ready.Transitions["start"]
			switch failure {
			case phaseGate:
				transition.Gate = func(_ context.Context,
					view fsm.Snapshot[businessData], payload command) error {

					view.Data.Values[0] = 99
					payload.Values[0] = 99
					return errInjected
				}
			case phaseUpdate:
				transition.Update = func(_ context.Context,
					view fsm.Snapshot[businessData], payload command) (businessData, error) {

					view.Data.Values[0] = 99
					payload.Values[0] = 99
					return view.Data, errInjected
				}
			case "validate":
				transition.Update = func(_ context.Context,
					view fsm.Snapshot[businessData], _ command) (businessData, error) {

					view.Data.Values[0] = -1
					return view.Data, nil
				}
			}
			ready.Transitions["start"] = transition
			cfg.States[stateReady] = ready
			machine := create(t, cfg)
			payload := command{Values: []int{2}}
			if _, err := machine.Send(t.Context(), "start", payload); err == nil {
				t.Fatal("expected rejection")
			}
			view := snapshot(t, machine)
			if view.Revision != 1 || view.State != stateReady ||
				!slices.Equal(view.Data.Values, []int{1}) ||
				payload.Values[0] != 2 || store.writes != 1 {

				t.Fatalf("rejection mutated state: %+v payload=%+v writes=%d", view, payload, store.writes)
			}
		})
	}
}

func TestCommitBeforeEffectAndCallbackIsolation(t *testing.T) {
	store := &memoryStore{}
	cfg := config(store)
	ready := cfg.States[stateReady]
	transition := ready.Transitions["start"]
	transition.Gate = func(_ context.Context, view fsm.Snapshot[businessData],
		payload command) error {

		view.Data.Values[0] = 90
		payload.Values[0] = 90
		return nil
	}
	ready.Transitions["start"] = transition
	cfg.States[stateReady] = ready
	var machine *fsm.Machine[businessData, command]
	pending := cfg.States[statePending]
	pending.Enter = func(ctx context.Context, view fsm.Snapshot[businessData]) error {
		record, err := store.Load(ctx, cfg.ID)
		if err != nil {
			return err
		}
		if record.State != statePending || record.Revision != 2 {
			return errors.New("effect ran before durable commit")
		}
		// Effects may inspect their committed local snapshot without taking
		// the event-serialization lock recursively.
		current, err := machine.Snapshot(ctx)
		if err != nil {
			return err
		}
		if current.Revision != view.Revision ||
			!slices.Equal(view.Data.Values, []int{1, 2}) {

			return fmt.Errorf("callback alias or stale snapshot: %+v", view)
		}
		view.Data.Values[0] = 80
		current.Data.Values[0] = 70
		return nil
	}
	cfg.States[statePending] = pending
	initial := businessData{Values: []int{1}}
	var err error
	machine, err = fsm.Create(t.Context(), cfg, stateReady, initial)
	if err != nil {
		t.Fatal(err)
	}
	initial.Values[0] = 60
	if err := machine.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's graph after construction cannot remove gates or
	// alter the graph that was validated by the engine.
	delete(cfg.States[stateReady].Transitions, "start")
	delete(cfg.States, statePending)
	payload := command{Values: []int{2}}
	committed, err := machine.Send(t.Context(), "start", payload)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(committed.Data.Values, []int{1, 2}) || payload.Values[0] != 2 {
		t.Fatalf("callback mutated another boundary: %+v %+v", committed, payload)
	}
	committed.Data.Values[0] = 50
	if got := snapshot(t, machine); !slices.Equal(got.Data.Values, []int{1, 2}) {
		t.Fatalf("returned snapshot aliases committed data: %+v", got)
	}
}

func TestAmbiguousCommitRequiresRecovery(t *testing.T) {
	for _, afterCommit := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%v", afterCommit), func(t *testing.T) {
			store := &memoryStore{}
			cfg := config(store)
			enters, restores := 0, 0
			pending := cfg.States[statePending]
			pending.Enter = func(context.Context, fsm.Snapshot[businessData]) error {
				enters++
				return nil
			}
			pending.Restore = func(context.Context, fsm.Snapshot[businessData]) error {
				restores++
				return nil
			}
			cfg.States[statePending] = pending
			machine := create(t, cfg)
			store.fail(afterCommit)
			view, err := machine.Send(t.Context(), "start", command{})
			assertError(t, err, errInjected)
			if enters != 0 || view.Revision != 0 {
				t.Fatalf("ambiguous commit ran effect or returned success: %+v", view)
			}
			_, err = machine.Send(t.Context(), "start", command{})
			assertError(t, err, fsm.ErrRecoveryRequired)
			store.repair()
			if err := machine.Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			view = snapshot(t, machine)
			if afterCommit {
				if view.State != statePending || view.Revision != 2 || restores != 1 {
					t.Fatalf("lost acknowledged checkpoint: %+v restores=%d", view, restores)
				}
			} else if view.State != stateReady || view.Revision != 1 || restores != 0 {
				t.Fatalf("uncommitted transition survived recovery: %+v restores=%d", view, restores)
			}
		})
	}
}

func TestStaleWriterIsFenced(t *testing.T) {
	store := &memoryStore{}
	cfg := config(store)
	first := create(t, cfg)
	second, err := fsm.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Send(t.Context(), "start", command{}); err != nil {
		t.Fatal(err)
	}
	_, err = second.Send(t.Context(), "start", command{})
	assertError(t, err, fsm.ErrConflict)
	_, err = second.Send(t.Context(), "start", command{})
	assertError(t, err, fsm.ErrRecoveryRequired)
	if err := second.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, second); got.State != statePending || got.Revision != 2 {
		t.Fatalf("recovery failed to reload winning revision: %+v", got)
	}
}

func TestRestartReconcilesInsteadOfReplayingEntry(t *testing.T) {
	store := &memoryStore{}
	cfg := config(store)
	payments, reconciliations := 0, 0
	pending := cfg.States[statePending]
	pending.Enter = func(_ context.Context, view fsm.Snapshot[businessData]) error {
		if view.Data.Operation != paymentOperation {
			return errors.New("effect intent was not persisted")
		}
		payments++
		// The remote effect succeeded, but the acknowledgement was lost.
		return errInjected
	}
	pending.Restore = func(_ context.Context, view fsm.Snapshot[businessData]) error {
		if payments != 1 || view.Data.Operation != paymentOperation {
			return errors.New("cannot reconcile durable intent")
		}
		reconciliations++
		return nil
	}
	cfg.States[statePending] = pending
	machine := create(t, cfg)
	committed, err := machine.Send(t.Context(), "start", command{})
	assertError(t, err, errInjected)
	if committed.State != statePending || committed.Revision != 2 {
		t.Fatalf("effect failure lost durable transition: %+v", committed)
	}
	_, err = machine.Send(t.Context(), "complete", command{})
	assertError(t, err, fsm.ErrRecoveryRequired)
	restarted, err := fsm.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if payments != 1 || reconciliations != 0 {
		t.Fatal("Open executed callbacks")
	}
	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Send(t.Context(), "complete", command{}); err != nil {
		t.Fatal(err)
	}
	if payments != 1 || reconciliations != 1 {
		t.Fatalf("recovery replayed payment: payments=%d restores=%d", payments, reconciliations)
	}
}

func TestRecoveryFailureBlocksDispatch(t *testing.T) {
	store := &memoryStore{}
	cfg := config(store)
	failRestore := true
	ready := cfg.States[stateReady]
	ready.Restore = func(context.Context, fsm.Snapshot[businessData]) error {
		if failRestore {
			return errInjected
		}
		return nil
	}
	cfg.States[stateReady] = ready
	machine, err := fsm.Create(t.Context(), cfg, stateReady, businessData{Values: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	assertError(t, machine.Recover(t.Context()), errInjected)
	_, err = machine.Send(t.Context(), "start", command{})
	assertError(t, err, fsm.ErrRecoveryRequired)
	failRestore = false
	if err := machine.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.Send(t.Context(), "start", command{}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.loadErr = errInjected
	store.mu.Unlock()
	assertError(t, machine.Recover(t.Context()), errInjected)
	_, err = machine.Send(t.Context(), "complete", command{})
	assertError(t, err, fsm.ErrRecoveryRequired)
}

func TestQueuedCancellation(t *testing.T) {
	store := &memoryStore{}
	cfg := config(store)
	entered, release := make(chan struct{}), make(chan struct{})
	pending := cfg.States[statePending]
	pending.Enter = func(context.Context, fsm.Snapshot[businessData]) error {
		close(entered)
		<-release
		return nil
	}
	cfg.States[statePending] = pending
	machine := create(t, cfg)
	finished := make(chan error, 1)
	go func() {
		_, err := machine.Send(t.Context(), "start", command{})
		finished <- err
	}()
	var releaseOnce sync.Once
	releaseEffect := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseEffect()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("entry was not called")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := make(chan error, 1)
	go func() {
		_, err := machine.Send(ctx, "complete", command{})
		canceled <- err
	}()
	select {
	case err := <-canceled:
		t.Fatalf("queued call did not wait for effect: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-canceled:
		assertError(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled caller blocked behind effect")
	}
	// Snapshot also remains available while an entry effect holds admission.
	if view := snapshot(t, machine); view.State != statePending || view.Revision != 2 {
		t.Fatalf("unexpected committed view: %+v", view)
	}
	releaseEffect()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("entry did not finish after release")
	}
}

func TestCallbackPanicFencesMachine(t *testing.T) {
	for _, phase := range []string{phaseGate, phaseUpdate, phaseEnter, phaseRestore} {
		t.Run(phase, func(t *testing.T) {
			store := &memoryStore{}
			cfg := config(store)
			ready := cfg.States[stateReady]
			transition := ready.Transitions["start"]
			pending := cfg.States[statePending]
			panicNow := true
			switch phase {
			case phaseGate:
				transition.Gate = func(context.Context, fsm.Snapshot[businessData], command) error {
					panic("injected callback panic")
				}
			case phaseUpdate:
				transition.Update = func(context.Context, fsm.Snapshot[businessData], command) (businessData, error) {
					panic("injected callback panic")
				}
			case phaseEnter:
				pending.Enter = func(context.Context, fsm.Snapshot[businessData]) error {
					panic("injected callback panic")
				}
			case phaseRestore:
				ready.Restore = func(context.Context, fsm.Snapshot[businessData]) error {
					if panicNow {
						panic("injected callback panic")
					}
					return nil
				}
			}
			ready.Transitions["start"] = transition
			cfg.States[stateReady], cfg.States[statePending] = ready, pending
			machine, err := fsm.Create(t.Context(), cfg, stateReady, businessData{Values: []int{1}})
			if err != nil {
				t.Fatal(err)
			}
			if phase != phaseRestore {
				if err := machine.Recover(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			panicked := false
			func() {
				defer func() { panicked = recover() != nil }()
				if phase == phaseRestore {
					_ = machine.Recover(t.Context())
				} else {
					_, _ = machine.Send(t.Context(), "start", command{})
				}
			}()
			if !panicked {
				t.Fatal("callback panic was swallowed")
			}
			_, err = machine.Send(t.Context(), "start", command{})
			assertError(t, err, fsm.ErrRecoveryRequired)
			panicNow = false
			if err := machine.Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			view := snapshot(t, machine)
			expectedRevision := uint64(1)
			if phase == phaseEnter {
				expectedRevision = 2
			}
			if view.Revision != expectedRevision {
				t.Fatalf("panic changed commit boundary: %+v", view)
			}
		})
	}
}

func TestInvalidDefinitionsAndRecords(t *testing.T) {
	mutations := map[string]func(*fsm.Config[businessData, command]){
		"empty id":            func(cfg *fsm.Config[businessData, command]) { cfg.ID = "" },
		"zero version":        func(cfg *fsm.Config[businessData, command]) { cfg.Version = 0 },
		"nil store":           func(cfg *fsm.Config[businessData, command]) { cfg.Store = nil },
		"missing data codec":  func(cfg *fsm.Config[businessData, command]) { cfg.DataCodec.Decode = nil },
		"missing event codec": func(cfg *fsm.Config[businessData, command]) { cfg.EventCodec.Encode = nil },
		"negative limit":      func(cfg *fsm.Config[businessData, command]) { cfg.MaxBytes = -1 },
		"missing restore": func(cfg *fsm.Config[businessData, command]) {
			state := cfg.States[stateReady]
			state.Restore = nil
			cfg.States[stateReady] = state
		},
		"missing validator": func(cfg *fsm.Config[businessData, command]) {
			state := cfg.States[stateDone]
			state.Validate = nil
			cfg.States[stateDone] = state
		},
		"missing gate": func(cfg *fsm.Config[businessData, command]) {
			transition := cfg.States[stateReady].Transitions["start"]
			transition.Gate = nil
			cfg.States[stateReady].Transitions["start"] = transition
		},
		"missing update": func(cfg *fsm.Config[businessData, command]) {
			transition := cfg.States[stateReady].Transitions["start"]
			transition.Update = nil
			cfg.States[stateReady].Transitions["start"] = transition
		},
		"undefined destination": func(cfg *fsm.Config[businessData, command]) {
			transition := cfg.States[stateReady].Transitions["start"]
			transition.To = "missing"
			cfg.States[stateReady].Transitions["start"] = transition
		},
		"terminal effect": func(cfg *fsm.Config[businessData, command]) {
			state := cfg.States[stateDone]
			state.Enter = noop
			cfg.States[stateDone] = state
		},
		"terminal restore": func(cfg *fsm.Config[businessData, command]) {
			state := cfg.States[stateDone]
			state.Restore = noop
			cfg.States[stateDone] = state
		},
		"terminal transition": func(cfg *fsm.Config[businessData, command]) {
			state := cfg.States[stateDone]
			state.Transitions = cfg.States[stateReady].Transitions
			cfg.States[stateDone] = state
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			store := &memoryStore{}
			cfg := config(store)
			mutate(&cfg)
			_, err := fsm.Create(t.Context(), cfg, stateReady, businessData{Values: []int{1}})
			assertError(t, err, fsm.ErrConfig)
			if store.writes != 0 {
				t.Fatal("invalid graph wrote a checkpoint")
			}
		})
	}
	recordMutations := map[string]func(*fsm.Record){
		"zero revision":     func(record *fsm.Record) { record.Revision = 0 },
		"overflow revision": func(record *fsm.Record) { record.Revision = fsm.MaxRevision + 1 },
		"wrong version":     func(record *fsm.Record) { record.Version++ },
		"unknown state":     func(record *fsm.Record) { record.State = "unknown" },
		"invalid encoding":  func(record *fsm.Record) { record.Data = []byte("{") },
		"invalid data":      func(record *fsm.Record) { record.Data = []byte(`{"Values":[-1]}`) },
	}
	for name, mutate := range recordMutations {
		t.Run(name, func(t *testing.T) {
			store := &memoryStore{}
			cfg := config(store)
			_ = create(t, cfg)
			store.rewrite(mutate)
			_, err := fsm.Open(t.Context(), cfg)
			assertError(t, err, fsm.ErrInvalidRecord)
		})
	}
}

func TestRecoveryRejectsRewrittenHistory(t *testing.T) {
	for _, change := range []string{"data", "state", "rollback"} {
		t.Run(change, func(t *testing.T) {
			store := &memoryStore{}
			cfg := config(store)
			machine := create(t, cfg)
			if _, err := machine.Send(t.Context(), "start", command{}); err != nil {
				t.Fatal(err)
			}
			store.rewrite(func(record *fsm.Record) {
				switch change {
				case "data":
					record.Data = []byte(`{"Values":[8]}`)
				case "state":
					record.State = stateReady
				case "rollback":
					record.Revision--
				}
			})
			assertError(t, machine.Recover(t.Context()), fsm.ErrInvalidRecord)
			_, err := machine.Send(t.Context(), "complete", command{})
			assertError(t, err, fsm.ErrRecoveryRequired)
		})
	}
}

func TestRevisionExhaustion(t *testing.T) {
	store := &memoryStore{}
	cfg := config(store)
	_ = create(t, cfg)
	store.rewrite(func(record *fsm.Record) { record.Revision = fsm.MaxRevision })
	machine, err := fsm.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = machine.Send(t.Context(), "start", command{})
	assertError(t, err, fsm.ErrRevisionExhausted)
	if store.writes != 1 {
		t.Fatal("exhausted revision reached store")
	}
}

// cancelingStore cancels the call after a successful durable write. This models
// shutdown arriving after COMMIT but before the caller can start its effect.
type cancelingStore struct {
	*memoryStore

	cancel context.CancelFunc
}

func (s *cancelingStore) CompareAndSwap(ctx context.Context, next fsm.Record,
	expected uint64) error {

	if err := s.memoryStore.CompareAndSwap(ctx, next, expected); err != nil {
		return err
	}
	s.cancel()
	return nil
}

func TestCancellationAfterCommitPreservesCheckpoint(t *testing.T) {
	store := &memoryStore{}
	cfg := config(store)
	entered := false
	pending := cfg.States[statePending]
	pending.Enter = func(context.Context, fsm.Snapshot[businessData]) error {
		entered = true
		return nil
	}
	cfg.States[statePending] = pending
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cfg.Store = &cancelingStore{memoryStore: store, cancel: cancel}
	machine := create(t, cfg)
	committed, err := machine.Send(ctx, "start", command{})
	assertError(t, err, context.Canceled)
	assertError(t, err, fsm.ErrRecoveryRequired)
	if entered || committed.State != statePending || committed.Revision != 2 {
		t.Fatalf("unexpected canceled commit: entered=%v snapshot=%+v", entered, committed)
	}
	_, err = machine.Send(t.Context(), "complete", command{})
	assertError(t, err, fsm.ErrRecoveryRequired)
	if err := machine.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, machine); got.State != statePending || got.Revision != 2 {
		t.Fatalf("recovery lost canceled commit: %+v", got)
	}
}

func TestJSONCodecRejectsIncompatibleInput(t *testing.T) {
	codec := fsm.JSONCodec[businessData]()
	for _, encoded := range []string{
		`{"Values":[1],"unknown":true}`,
		`{"Values":[1],"Values":[2]}`,
		`{"Values":"wrong type"}`,
		`{"Values":[1]} {"Values":[2]}`,
	} {
		if _, err := codec.Decode([]byte(encoded)); err == nil {
			t.Fatalf("accepted invalid JSON: %s", encoded)
		}
	}
}

func TestSizeLimitsPrecedeCallbacksAndWrites(t *testing.T) {
	store := &memoryStore{}
	cfg := config(store)
	cfg.MaxBytes = 100
	gates := 0
	ready := cfg.States[stateReady]
	transition := ready.Transitions["start"]
	transition.Gate = func(context.Context, fsm.Snapshot[businessData], command) error {
		gates++
		return nil
	}
	ready.Transitions["start"] = transition
	cfg.States[stateReady] = ready
	machine := create(t, cfg)
	_, err := machine.Send(t.Context(), "start", command{Values: make([]int, 100)})
	assertError(t, err, fsm.ErrEventRejected)
	if gates != 0 || store.writes != 1 {
		t.Fatalf("oversized event reached callback/write: gates=%d writes=%d", gates, store.writes)
	}

	store.rewrite(func(record *fsm.Record) { record.Data = make([]byte, 101) })
	assertError(t, machine.Recover(t.Context()), fsm.ErrInvalidRecord)
	_, err = machine.Send(t.Context(), "start", command{})
	assertError(t, err, fsm.ErrRecoveryRequired)
}

func TestAmbiguousCreationCanBeResolvedByOpen(t *testing.T) {
	store := &memoryStore{}
	store.fail(true)
	cfg := config(store)
	machine, err := fsm.Create(t.Context(), cfg, stateReady, businessData{Values: []int{1}})
	assertError(t, err, errInjected)
	if machine != nil {
		t.Fatal("failed Create returned usable machine")
	}
	store.repair()
	machine, err = fsm.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, machine); got.Revision != 1 || got.State != stateReady {
		t.Fatalf("Open did not resolve ambiguous creation: %+v", got)
	}
	_, err = fsm.Create(t.Context(), cfg, stateReady, businessData{Values: []int{2}})
	assertError(t, err, fsm.ErrConflict)
}
