// Package fsm provides an opt-in, durable finite state machine. It commits typed
// business updates before running effects and requires explicit recovery before
// accepting events. See README.md for the storage and ownership contracts.
package fsm

import (
	"context"
	"errors"
	"math"
)

var (
	// ErrNotFound means no record exists for the machine ID.
	ErrNotFound = errors.New("fsm record not found")
	// ErrConflict means creation or compare-and-swap lost to another writer.
	ErrConflict = errors.New("fsm revision conflict")
	// ErrConfig means the definition or dependencies are invalid.
	ErrConfig = errors.New("invalid fsm configuration")
	// ErrInvalidRecord means a checkpoint failed envelope or data validation.
	ErrInvalidRecord = errors.New("invalid fsm record")
	// ErrEventRejected means the current state does not accept the event.
	ErrEventRejected = errors.New("fsm event rejected")
	// ErrRecoveryRequired means events are blocked until Recover succeeds.
	ErrRecoveryRequired = errors.New("fsm recovery required")
	// ErrRevisionExhausted prevents revision rollover and ABA writes.
	ErrRevisionExhausted = errors.New("fsm revision exhausted")
)

const (
	// MaxRevision is bounded by SQL's signed BIGINT representation.
	MaxRevision uint64 = math.MaxInt64
	// DefaultMaxBytes bounds each encoded data snapshot and event payload.
	DefaultMaxBytes = 1 << 20
)

// StateID and Event name states and commands within a versioned definition.
type StateID string

type Event string

// Record is the complete durable checkpoint. Store implementations must preserve
// all fields atomically. Data contains business state and durable effect intent,
// never live sessions, channels, private nonces or callback closures.
type Record struct {
	ID       string
	Version  uint32
	Revision uint64
	State    StateID
	Data     []byte
}

// Store supplies a linearizable per-ID durable register. Successful writes must
// survive process restart. A write error may have committed; the engine fences
// itself and requires recovery instead of assuming rollback. Implementations
// must own/copy Data, never retain or return mutable aliases, never delete and
// reuse IDs, and be safe for concurrent calls. CompareAndSwap must atomically
// replace every field only when the ID exists at expectedRevision, and require
// next.Revision == expectedRevision+1. Create requires revision 1 and must not
// overwrite an existing ID. Missing/conflicting writes return ErrConflict.
// Storage configuration (fsync, replication, transactions) is part of this
// contract; an in-memory map is suitable only for tests.
type Store interface {
	Create(context.Context, Record) error
	Load(context.Context, string) (Record, error)
	CompareAndSwap(ctx context.Context, next Record,
		expectedRevision uint64) error
}

// Codec isolates ownership at every callback boundary. Decode must allocate an
// independent value on every call; neither function may retain mutable aliases
// or have side effects. Both functions must be safe for concurrent calls, since
// Snapshot can decode while Send/Recover is executing. Codecs and business
// callbacks are trusted code.
type Codec[T any] struct {
	Encode func(T) ([]byte, error)
	Decode func([]byte) (T, error)
}

// Snapshot is a detached, typed view of one committed revision. Mutating its
// Data does not change the machine or any other callback's snapshot.
type Snapshot[D any] struct {
	ID       string
	Version  uint32
	Revision uint64
	State    StateID
	Data     D
}

// Gate authorizes a transition against the committed business state. It must be
// side-effect-free; an error rejects the event without writing any state.
type Gate[D, E any] func(context.Context, Snapshot[D], E) error

// Update computes candidate business state without external effects. Its input
// is isolated from the gate's input. The destination validator checks the
// encoded result before commit. Persist operation IDs and effect intent here.
type Update[D, E any] func(context.Context, Snapshot[D], E) (D, error)

// Effect reconciles or executes work described by an already committed snapshot.
// It cannot update durable state directly through the engine. Send a subsequent
// event after it returns to record completion. Effects must respect cancellation
// and use business-specific idempotency. Keep effects bounded: start supervised
// workers for long-lived monitoring and return after registration. They must
// not re-enter Send or Recover
// on the same machine, which serializes these calls through callback completion.
type Effect[D any] func(context.Context, Snapshot[D]) error

// Transition requires an explicit gate and update, including for self-loops.
type Transition[D, E any] struct {
	To     StateID
	Gate   Gate[D, E]
	Update Update[D, E]
}

// State defines validation, recovery and permitted commands. Validate is pure
// and required for every state. Nonterminal states require Restore, which must
// reconstruct ephemeral runtime and reconcile possibly completed effects.
// Enter is optional and runs only after a transition commits. Recover never
// calls Enter. Terminal states have no transitions, Enter or Restore: complete
// all business/resource reconciliation before committing a terminal state.
type State[D, E any] struct {
	Terminal    bool
	Validate    func(D) error
	Restore     Effect[D]
	Enter       Effect[D]
	Transitions map[Event]Transition[D, E]
}

// Config is copied and validated on Create/Open. Version binds the graph, codec
// schema and recovery semantics; incompatible changes require a new version and
// an explicit offline/transactional migration. MaxBytes defaults to 1 MiB.
// Exactly one runtime owner may execute effects for an ID. CAS protects state
// writes, not external effects; deployments need exclusive ownership or external
// leases with fencing enforced at effect destinations.
type Config[D, E any] struct {
	ID         string
	Version    uint32
	Store      Store
	DataCodec  Codec[D]
	EventCodec Codec[E]
	States     map[StateID]State[D, E]
	MaxBytes   int
}
