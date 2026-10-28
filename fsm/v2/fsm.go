package fsm

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"sync"
)

// Machine serializes transitions for one durable ID. Do not copy a Machine.
// The serialization token is held through callbacks; cancellation can interrupt
// admission, but trusted callbacks themselves must honor their context.
type Machine[D, E any] struct {
	cfg   Config[D, E]
	token chan struct{}
	// ready is accessed only while holding token.
	ready  bool
	mu     sync.RWMutex
	record Record
}

// Create stores revision 1 without executing business callbacks. The returned
// machine requires Recover, just like Open. Initial state must therefore have a
// restore path that is safe whether initialization has run before or not.
// A creation error may have committed: Open the same ID to resolve it, never
// create a replacement ID to retry a financial operation.
func Create[D, E any](ctx context.Context, cfg Config[D, E],
	initial StateID, data D) (*Machine[D, E], error) {

	m, err := configure(cfg)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	encoded, err := m.encode(data)
	if err != nil {
		return nil, err
	}
	record := Record{
		ID: cfg.ID, Version: cfg.Version, Revision: 1,
		State: initial, Data: encoded,
	}
	if err := m.validate(record); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cfg.Store.Create(ctx, cloneRecord(record)); err != nil {
		return nil, fmt.Errorf("create checkpoint: %w", err)
	}
	m.setRecord(record)
	return m, nil
}

// Open loads a checkpoint and validates its version, state and business data.
// It executes no effects. Recover must succeed before the first Send.
func Open[D, E any](ctx context.Context,
	cfg Config[D, E]) (*Machine[D, E], error) {

	m, err := configure(cfg)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record, err := cfg.Store.Load(ctx, cfg.ID)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}
	record = cloneRecord(record)
	if err := m.validate(record); err != nil {
		return nil, err
	}
	m.setRecord(record)
	return m, nil
}

func configure[D, E any](cfg Config[D, E]) (*Machine[D, E], error) {
	if cfg.ID == "" || cfg.Version == 0 || cfg.Store == nil ||
		cfg.DataCodec.Encode == nil || cfg.DataCodec.Decode == nil ||
		cfg.EventCodec.Encode == nil || cfg.EventCodec.Decode == nil ||
		len(cfg.States) == 0 || cfg.MaxBytes < 0 {

		return nil, fmt.Errorf("%w: missing dependency or invalid limit", ErrConfig)
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	states := maps.Clone(cfg.States)
	for id, state := range states {
		if id == "" || state.Validate == nil {
			return nil, fmt.Errorf("%w: state needs name and validator", ErrConfig)
		}
		if state.Terminal {
			if len(state.Transitions) != 0 || state.Enter != nil ||
				state.Restore != nil {

				return nil, fmt.Errorf("%w: terminal state has work", ErrConfig)
			}
		} else if state.Restore == nil {
			return nil, fmt.Errorf("%w: nonterminal state needs restore", ErrConfig)
		}
		state.Transitions = maps.Clone(state.Transitions)
		for event, transition := range state.Transitions {
			if _, ok := states[transition.To]; !ok || event == "" ||
				transition.Gate == nil || transition.Update == nil {

				return nil, fmt.Errorf("%w: invalid transition", ErrConfig)
			}
		}
		states[id] = state
	}
	cfg.States = states
	m := &Machine[D, E]{cfg: cfg, token: make(chan struct{}, 1)}
	m.token <- struct{}{}
	return m, nil
}

// Recover reloads the authoritative checkpoint and invokes its dedicated Restore
// callback, never Enter or a synthetic transition. It is retryable and must be
// idempotent. A failed/canceled restore leaves dispatch blocked. Applications
// must quiesce previous runtime subscriptions before starting new ones here.
func (m *Machine[D, E]) Recover(ctx context.Context) error {
	if err := m.acquire(ctx); err != nil {
		return err
	}
	defer m.release()
	m.ready = false
	record, err := m.cfg.Store.Load(ctx, m.cfg.ID)
	if err != nil {
		return fmt.Errorf("reload checkpoint: %w", err)
	}
	record = cloneRecord(record)
	if err := m.validate(record); err != nil {
		return err
	}
	previous := m.getRecord()
	if record.Revision < previous.Revision ||
		(record.Revision == previous.Revision &&
			!sameRecord(record, previous)) {

		return fmt.Errorf("%w: checkpoint regressed or changed without revision",
			ErrInvalidRecord)
	}
	m.setRecord(record)
	if err := ctx.Err(); err != nil {
		return err
	}
	state := m.cfg.States[record.State]
	if !state.Terminal {
		snapshot, err := m.snapshot(record)
		if err != nil {
			return err
		}
		if err := state.Restore(ctx, snapshot); err != nil {
			return fmt.Errorf("restore state: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.ready = true
	return nil
}

// Send processes exactly one transition. Gate, Update and destination validation
// precede the atomic checkpoint write; Enter follows it. Payloads and data are
// independently decoded for each callback. Rejected events have no effects.
//
// On a persistence error, the outcome is unknown and the returned snapshot is
// zero; Recover is mandatory. Once committed, the returned snapshot identifies
// the committed revision even if Enter fails or the context is canceled. Such
// errors wrap ErrRecoveryRequired, and subsequent Send calls are blocked until
// recovery succeeds. Callback panics propagate and also block further dispatch.
func (m *Machine[D, E]) Send(ctx context.Context, event Event,
	payload E) (Snapshot[D], error) {

	var zero Snapshot[D]
	if err := m.acquire(ctx); err != nil {
		return zero, err
	}
	defer m.release()
	// Fail closed on panics in trusted callbacks, codecs or storage. Do not turn
	// a programmer error into a successful financial transition.
	returned := false
	defer func() {
		if !returned {
			m.ready = false
		}
	}()
	snapshot, err := m.send(ctx, event, payload)
	returned = true
	return snapshot, err
}

func (m *Machine[D, E]) send(ctx context.Context, event Event,
	payload E) (Snapshot[D], error) {

	var zero Snapshot[D]
	if !m.ready {
		return zero, ErrRecoveryRequired
	}
	previous := m.getRecord()
	transition, ok := m.cfg.States[previous.State].Transitions[event]
	if !ok {
		return zero, ErrEventRejected
	}
	if previous.Revision == MaxRevision {
		return zero, ErrRevisionExhausted
	}
	encodedEvent, err := m.cfg.EventCodec.Encode(payload)
	if err != nil {
		return zero, fmt.Errorf("encode event: %w", err)
	}
	if len(encodedEvent) > m.cfg.MaxBytes {
		return zero, fmt.Errorf("%w: event too large", ErrEventRejected)
	}
	encodedEvent = bytes.Clone(encodedEvent)
	before, err := m.snapshot(previous)
	if err != nil {
		return zero, err
	}
	gatePayload, err := m.cfg.EventCodec.Decode(bytes.Clone(encodedEvent))
	if err != nil {
		return zero, fmt.Errorf("decode event for gate: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := transition.Gate(ctx, before, gatePayload); err != nil {
		return zero, fmt.Errorf("transition gate: %w", err)
	}
	before, err = m.snapshot(previous)
	if err != nil {
		return zero, err
	}
	updatePayload, err := m.cfg.EventCodec.Decode(bytes.Clone(encodedEvent))
	if err != nil {
		return zero, fmt.Errorf("decode event for update: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	updated, err := transition.Update(ctx, before, updatePayload)
	if err != nil {
		return zero, fmt.Errorf("transition update: %w", err)
	}
	encoded, err := m.encode(updated)
	if err != nil {
		return zero, err
	}
	next := Record{
		ID: previous.ID, Version: previous.Version,
		Revision: previous.Revision + 1, State: transition.To, Data: encoded,
	}
	if err := m.validate(next); err != nil {
		return zero, err
	}
	// Decode both views before commit so a codec failure cannot hide a known
	// committed revision. The effect gets a separate value from the caller.
	result, err := m.snapshot(next)
	if err != nil {
		return zero, err
	}
	effectView, err := m.snapshot(next)
	if err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	// Fence before starting I/O: even a panic or canceled commit can have written.
	m.ready = false
	if err := m.cfg.Store.CompareAndSwap(ctx, cloneRecord(next),
		previous.Revision); err != nil {
		return zero, fmt.Errorf("%w: commit checkpoint: %w",
			ErrRecoveryRequired, err)
	}
	m.setRecord(next)
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("%w: %w", ErrRecoveryRequired, err)
	}
	if enter := m.cfg.States[next.State].Enter; enter != nil {
		if err := enter(ctx, effectView); err != nil {
			return result, fmt.Errorf("%w: enter state: %w",
				ErrRecoveryRequired, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("%w: %w", ErrRecoveryRequired, err)
	}
	m.ready = true
	return result, nil
}

// Snapshot returns a detached view of the latest locally known committed
// checkpoint, including while an effect is running. It does not imply that
// effects completed, that recovery succeeded, or that another writer has not
// advanced the store. It never publishes an uncommitted candidate.
func (m *Machine[D, E]) Snapshot(ctx context.Context) (Snapshot[D], error) {
	if err := ctx.Err(); err != nil {
		return Snapshot[D]{}, err
	}
	return m.snapshot(m.getRecord())
}

func (m *Machine[D, E]) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
		if err := ctx.Err(); err != nil {
			m.release()
			return err
		}
		return nil
	}
}

func (m *Machine[D, E]) release() {
	m.token <- struct{}{}
}

func (m *Machine[D, E]) getRecord() Record {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneRecord(m.record)
}

func (m *Machine[D, E]) setRecord(record Record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record = cloneRecord(record)
}

func cloneRecord(record Record) Record {
	record.Data = bytes.Clone(record.Data)
	return record
}

func sameRecord(a, b Record) bool {
	return a.ID == b.ID && a.Version == b.Version &&
		a.Revision == b.Revision && a.State == b.State &&
		bytes.Equal(a.Data, b.Data)
}

func (m *Machine[D, E]) snapshot(record Record) (Snapshot[D], error) {
	data, err := m.cfg.DataCodec.Decode(bytes.Clone(record.Data))
	if err != nil {
		return Snapshot[D]{}, fmt.Errorf("%w: decode business state: %w",
			ErrInvalidRecord, err)
	}
	return Snapshot[D]{
		ID: record.ID, Version: record.Version, Revision: record.Revision,
		State: record.State, Data: data,
	}, nil
}

func (m *Machine[D, E]) encode(data D) ([]byte, error) {
	encoded, err := m.cfg.DataCodec.Encode(data)
	if err != nil {
		return nil, fmt.Errorf("encode business state: %w", err)
	}
	if len(encoded) == 0 || len(encoded) > m.cfg.MaxBytes {
		return nil, fmt.Errorf("%w: business state size", ErrInvalidRecord)
	}
	return bytes.Clone(encoded), nil
}

func (m *Machine[D, E]) validate(record Record) error {
	state, ok := m.cfg.States[record.State]
	if record.ID != m.cfg.ID || record.Version != m.cfg.Version ||
		record.Revision == 0 || !ok ||
		len(record.Data) == 0 || len(record.Data) > m.cfg.MaxBytes {

		return fmt.Errorf("%w: checkpoint envelope", ErrInvalidRecord)
	}
	snapshot, err := m.snapshot(record)
	if err != nil {
		return err
	}
	if err := state.Validate(snapshot.Data); err != nil {
		return fmt.Errorf("%w: business invariant: %w", ErrInvalidRecord, err)
	}
	return nil
}
