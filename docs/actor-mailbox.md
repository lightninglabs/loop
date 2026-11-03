# Independent actor and mailbox design

This is a design for a separate runtime; no actor/mailbox implementation is added
by FSM v2. The runtime can ship and be adopted independently of the FSM, its
store contract, or any domain data changes. A bounded in-memory mailbox needs no
database schema or migration.

## Package boundaries and adoption

A reusable `actor` package would own message admission, a bounded mailbox,
sequential handler execution, replies, and joined shutdown. It would have no
imports of `fsm`, `fsm/v2`, `loopdb`, Instant Out, or static-address packages.
Business commands, protocol decisions, signing, and database writes stay in the
consumer. The FSM core likewise does not depend on an actor runtime.

An Instant Out or static-address manager can provide a handler adapter around the
original FSM, FSM v2, or its existing service methods. Introducing the runtime
therefore does not require converting persisted records or deploying FSM v2.
Adopt it one owner at a time and route every mutation through that owner; wrapping
one method while retaining other direct mutable access does not establish actor
ownership. Expose immutable snapshots for reads.

Wrapping a legacy FSM does not repair its persistence ordering or restart rules.
Long-running legacy actions can also monopolize the actor until they return.
Moving them behind a mailbox still requires bounded handlers or supervised
workers so urgent timeout and chain events can progress.

The packages would have distinct responsibilities:

| Layer | Responsibility | Database migration needed? |
| --- | --- | --- |
| Independent actor/mailbox | Runtime ownership, bounded admission, serialized handlers, replies and shutdown | No for an in-memory mailbox |
| FSM v2 core | Gates, business updates, commit ordering and per-state restoration | No schema requirement |
| Per-flow store adapter | Map logical FSM snapshots to existing domain records/history; compare and update atomically with resource changes | Flow-specific only if existing storage cannot express or enforce the contract |
| Optional durable inbox | Persisted command acceptance, identity, deduplication and replay | Separate per-flow storage and transaction design |
| Domain manager | Signing/payment rules, resource ownership, workers and recovery policy | Depends on the domain change |

## Admission, execution, and cancellation

A small runtime API can distinguish admission from waiting for completion:
`Request(admissionCtx, command)` returns a reply handle after admission;
`reply.Await(waitCtx)` waits for the result. A nonblocking admission variant can
return `ErrMailboxFull`. These names are proposed API, not available functions.

- An admission error means the command was not queued. A full mailbox applies
  backpressure or returns an explicit error; financial commands must not be
  silently dropped or overwritten.
- Successful in-memory admission is volatile and does not acknowledge a durable
  financial obligation. The handler result must state the actual business
  outcome, including any uncertain external effect or committed transition.
- Canceling a reply wait does not cancel an already admitted operation. The
  handler uses the owner's lifecycle context. Operation cancellation, when the
  protocol permits it, is an explicit domain command with its own state rules.
- Each request has a buffered single-completion reply or equivalent promise.
  An abandoned caller must never block the owner while it delivers a result.
- Messages and replies transfer ownership or copy mutable data. Callers cannot
  retain a mutable alias to actor-owned transactions, slices, maps, or sessions.

Handlers execute sequentially and must not synchronously await another command
on the same actor. Long I/O and monitoring belong in a bounded set of owned
workers. Workers post immutable observations with an operation ID or generation;
the owner checks that identity before applying a late result to current state.
Worker failures must reach the domain supervisor rather than disappearing when
the original RPC caller cancels.

## Startup, shutdown, and supervision

The supervisor supplies the runtime lifetime and joins termination. For a v2
consumer, startup loads and restores the machine before admitting normal
commands. A legacy consumer retains its existing business recovery routine.
Neither routine belongs in the generic actor package.

On shutdown, stop admission, resolve queued requests according to an explicit
policy, cancel active handlers and owned workers, and join them before signaling
`Done`. If a handler ignores cancellation, shutdown cannot safely report that it
has stopped; a stop request and completed shutdown are distinct events. Define
whether admitted commands are drained or rejected and test both the queue and
an active request at the boundary.

A panic or uncertain financial effect must not trigger an automatic blind replay
of the last handler. The domain supervisor decides whether to reload, reconcile,
restore, or surface an actionable failure. Restarts must not leave old workers
or signer sessions running alongside their replacements.

## Durable command acceptance

A persistent mailbox is a separate capability. Durable acceptance requires a
stable command ID, payload/version rules, an inbox record, deduplication, replay
policy, and completion tracking. Its storage representation is a separate
per-flow decision. Commit inbox processing consistently with the existing domain
records, history, and resource changes. External payments, publications, and
signature release still need durable intent and protocol-specific reconciliation;
a transactional inbox cannot provide exactly-once remote execution.

An actor serializes one process's handlers. It does not provide atomic ownership
of deposits, distributed process fencing, or durability of an in-memory queue.
Those guarantees stay in the domain store and deployment ownership protocol.

## Runtime acceptance tests

Before production adoption, test full-mailbox backpressure; cancellation before
and after admission; abandoned reply waiters; concurrent producers; mutable
message isolation; shutdown with queued and active requests; worker joins; stale
worker results; handler panic; and supervisor-driven recovery. Also retain the
consumer's crash tests around every durable commit and irreversible effect.
