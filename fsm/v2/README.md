# Durable FSM v2

`github.com/lightninglabs/loop/fsm/v2` is an opt-in foundation for swap state
machines. It separates transition decisions, durable updates, external effects,
and restart recovery. Static-address withdrawals are its first production
consumer; see [their workflow and migration design](../../staticaddr/withdraw/README.md).
Instant Out and static-address Loop In retain the legacy engine and require
separate per-flow adoption and crash-boundary tests.

## Lessons from the existing implementation

These are observable ordering and recovery risks, not claims that a particular
failure has been exploited. Links refer to the existing implementation so a
consumer can check each boundary against the business protocol.

| Boundary | Existing behavior | Required adoption property |
| --- | --- | --- |
| State publication and persistence | [`getNextState`](../fsm.go#L202) changes in-memory state; [`SendEvent`](../fsm.go#L232) notifies observers before the persistence hook. The hook cannot return an error. | A state becomes available to effects and successful callers only after its durable commit. |
| Failed writes | [`updateInstantOut`](../../instantout/fsm.go#L335) and [`updateLoopIn`](../../staticaddr/loopin/fsm.go#L250) set business state, then log storage failures and return. | Failed or ambiguous commits stop progress and require reload and reconciliation. |
| Signature release | [`SignHtlcTxAction`](../../staticaddr/loopin/actions.go#L691) sends partial signatures before returning the event that enters the monitoring state. [`SignHtlcTx` recovery](../../staticaddr/loopin/fsm.go#L176) routes to `UnlockDeposits`. | Commit an intent meaning “the server may have signatures” before release; an uncertain RPC result must retain monitoring and ownership obligations. |
| Completing resources after payment | [`PaymentReceived` recovery](../../staticaddr/loopin/fsm.go#L210) goes directly to `Succeeded`, bypassing the [`deposit finalization action`](../../staticaddr/loopin/actions.go#L1477). | Restore must reconcile deposit finalization after a crash between state commit and action execution. |
| Transaction publication | Instant Out [`publishes before retaining its cooperative sweep`](../../instantout/actions.go#L568). Static-address Loop In [`publishes before retaining the timeout sweep hash`](../../staticaddr/loopin/actions.go#L1553). Persistence follows on the next transition. | Persist the exact signed transaction, destination, fee constraints, and publication intent before broadcasting. Restore reconciles that transaction. |
| Restore policy | Instant Out enables additional preimage-release checks through an [`ephemeral recovery event context`](../../instantout/actions.go#L442). Other states encode recovery as manually added self-transitions. | Every nonterminal state has a dedicated restore function that validates and reconstructs its runtime obligations. |
| Concurrent writers | Existing [`Instant Out`](../../instantout/store.go#L214) and [`Loop In`](../../staticaddr/loopin/sql_store.go#L383) transactions save data and history together, but their updates do not compare a durable FSM revision. | Reject stale writes using compare-and-swap; separately enforce a single owner for effects. |

The old observer's [`condition-variable wait`](../observer.go#L153) also has no
cancellation wake-up. A context check only after a notification cannot guarantee
that cancellation releases a blocked waiter. V2 avoids observer callbacks and
waiter goroutines in the transition engine.

## Transition contract

`Machine[D, E]` uses typed durable business data (`D`) and event payloads (`E`).
`StateID` and `Event` identify graph edges; they are distinct from payloads and
from machine identity. The definition is validated and copied at construction.
Changing caller-owned maps later must not change a running machine.

Each transition has an explicit target, a mandatory `Gate`, and a mandatory
`Update`. Each state has a mandatory `Validate`; nonterminal states additionally
require `Restore`. `Enter` is optional and runs only after the next record is
committed. Terminal states have no transitions or effects: finish cleanup in a
nonterminal state before entering a terminal state.

A transition proceeds in this order:

1. Acquire context-aware serialization and require a recovered machine.
2. Resolve the current state's event and run its `Gate` on an isolated snapshot.
3. Run `Update` on a separate snapshot to produce the next business data.
4. Validate the destination state and encoded data.
5. Atomically compare and replace the durable record with the next revision.
6. Make the committed snapshot current and run the destination's `Enter`.

Gates reject events without writes or effects. Updates prepare data and intents;
they must not pay invoices, sign, reveal secrets, publish transactions, or change
resource ownership outside the store transaction. Validators enforce business
invariants, including on records loaded after restart. Gates, updates, validators,
and codecs must be deterministic, side-effect-free code. Callback signatures
cannot enforce purity or prevent a closure from retaining shared mutable data.

Use the gate for transition-specific authorization and checks; use state
validation for invariants that must also hold on restart. Examples include exact
swap/deposit binding, supported protocol version, amount and fee bounds, required
fallback transactions, and durable evidence of payment before spend authorization.
An external observation can change after a gate runs. A gate is not an atomic
transaction with lnd, a remote server, or the chain; the effect must still enforce
its own protocol checks and handle uncertainty.

The logical `Record` binds machine `ID`, definition/data-contract `Version`,
monotonic `Revision`, `State`, and encoded `Data`. A store must atomically create
or compare-and-swap that complete logical snapshot. In particular, data and state
must not be separate commits. Each successful compare-and-swap increments the
machine revision by one. `Record` is an engine contract, not a SQL row layout.
Use a stable version for the graph and codec contract, and reject unsupported
versions rather than interpreting old data using a new graph. Incompatible
versions require an explicit per-flow compatibility or data-conversion decision;
the engine does not migrate stored business data.

Codecs provide ownership isolation through encode/decode boundaries, including
snapshots returned to callers and values passed to callbacks. A codec must not
return aliases into retained application state or its input bytes. Codecs must
also be safe for concurrent calls, since `Snapshot` may run during an effect. A
shallow struct copy is insufficient for pointers, maps, slices, and transaction
objects. The encoded snapshot must represent all information needed to resume
the durable obligation; event payloads and callback-local variables are not
durable storage. Encoding provides isolation inside the engine; it does not
require storing those bytes in an opaque-blob column or table.

There are no automatic action-generated event chains. The owner submits each
event explicitly and checks its result, making commit and effect boundaries
visible. Callbacks run synchronously under operation serialization and must obey
their context; they must not call serialized methods on the same machine. A
canceled caller waiting for serialization can return without starting an event.
Cancellation cannot forcibly stop a callback that ignores its context. Keep
callbacks bounded: register supervised runtime workers for long-lived monitoring
and return, so the owner can process urgent timeout and chain events. Worker
results should carry an intent identity or expected revision; reject stale
results in a gate rather than applying them to a later operation.

## Recovery and uncertain outcomes

Loading a record is not permission to resume effects. Reload validates identity,
version, revision, known state, decoded data, and state invariants, then invokes
that state's dedicated `Restore`. Normal events are accepted only after restore
succeeds. Restore rebuilds subscriptions and runtime state or reconciles effects
that may already have happened; it must not blindly repeat `Enter`.

If persistence reports an error, it may have committed before the error became
visible. The engine must not run the destination effect or continue from an
assumed old record. Reload the authoritative record, validate it, and restore its
state before accepting another event. The same recovery requirement applies
after an effect callback fails or a callback panics: some external work may
already have happened. Panics propagate to the caller; the engine does not log
panic values, event payloads, or durable data. A supervisor that catches panics
must protect secrets in its own diagnostics and arrange recovery.

Successful persistence followed by process death can leave `Enter` unexecuted,
partially executed, or completed without its next event being committed. Restore
must handle all three cases. The package does not promise exactly-once effects.
Use stable operation identifiers derived from durable machine/intent identity,
query external state, and make retries idempotent where the protocol permits.
Do not retry a financial operation solely because the previous RPC timed out.

For signing protocols, persist the obligation and immutable transaction context
before signature release. Do not persist secret MuSig2 nonces or resurrect signer
sessions across restarts; the existing Loop In FSM already calls out this
[`nonce/session restriction`](../../staticaddr/loopin/fsm.go#L23). Recovery must
choose a fresh protocol session, query the counterpart, monitor a possible spend,
or take the unilateral fallback according to the protocol. An FSM cannot make
nonce reuse safe.

## Storage, ownership, and actors

The engine depends only on the `Store` contract and prescribes no SQL backend,
checkpoint table, or database schema. Each flow implements its own adapter that
maps the logical `Record` to its existing domain records and history. On load the
adapter reconstructs and encodes the snapshot; on write it decodes the proposed
snapshot and writes the appropriate domain fields. The `Data` byte representation
is an ownership boundary for the engine, not a required persistence format.
`Load` must establish `Version` from the durable domain interpretation, not blindly
copy current `Config.Version` or assume a wire protocol version is interchangeable.
The adapter must validate any such mapping. Exclude transient runtime observations
from reconstructed `Data`; changing an included business field must advance its
revision.

For a flow using Loop's existing SQL stores, perform the expected-revision check,
business-state update, history append, and relevant deposit/reservation ownership
changes in the same existing transaction. A stale writer must be rejected before
any of those changes commit. `Load` must read state, artifacts, and revision from
one consistent transaction view. Revisions advance contiguously by one for each
machine; a globally allocated history update ID is not interchangeable. Existing
history can supply a revision only if the adapter enforces that contract and its
read, compare, and write are atomic under concurrent access. The existing
unchecked `Update*` methods do not become compare-and-swap merely by preceding
them with a read; a wall-clock timestamp alone is not a revision.

`Load` must return identical encoded `Data` for an unchanged revision; recovery
compares bytes, not semantic equality. Use a stable, versioned projection for
prepared updates and loads, with explicit row/map ordering, nil/empty rules, and
timestamp timezone/precision. `JSONCodec` uses deterministic marshaling, but Go's
guarantee applies only to identical binaries; it is not cross-build canonical
encoding or schema compatibility. Verify representation compatibility across
upgrades and define an explicit compatibility policy for graph, codec, or
projection changes. A physical data migration is a flow-specific option, not an
engine requirement.

Adding the engine does not require a database migration. Reuse existing fields
and history when they can express and enforce the flow's durable obligations.
If legacy data is unsupported, a required recovery artifact is missing, or the
current store cannot enforce atomic revision comparison, make a flow-specific
adoption decision. That may require explicit compatibility handling, additional
domain fields, or a domain migration reviewed with the affected protocol. The
withdrawal-specific adapter and reviewed legacy import implement this contract
for withdrawals. Other flows remain unchanged; the engine itself never moves
or reinterprets their records.

The generic engine remains in `fsm/v2/fsm.go`, with no database or actor imports.
Database schema versions and the FSM record's `Version` serve different purposes:
changing the storage schema does not migrate a business protocol or establish
that an old entry action completed. Durability settings, permissions, encryption,
backup/restore policy, and disk integrity remain responsibilities of each domain
store and its deployment. A logical record is not tamper-proof or protected
against rollback to an old backup merely because it has a revision.

Compare-and-swap prevents two writers from committing the same expected revision.
It does not fence a previously running process from performing an effect after
another process takes over. Run exactly one active owner per swap, including
restore. Multi-process takeover requires an external ownership protocol and
fencing supported by the affected service; the engine supplies neither a
distributed lease nor external-service fencing.

Swap state and resource ownership must remain consistent. If a resource cannot
share the domain store transaction, model durable reservation/reconciliation
states and make recovery idempotent rather than treating the resource change as
part of an atomic update. Calling an existing resource manager in `Update`
violates its purity contract; calling it in `Enter` still needs a recovery plan
for a crash between the two commits.

A per-swap actor can own the machine, signer sessions, subscriptions, and runtime
workers. A bounded mailbox and explicit request/reply messages can provide
backpressure and prevent callers from sharing mutable swap state. It is a useful
runtime model, but an in-memory mailbox is volatile. A reply that says “queued”
must not imply durable acceptance. Durable request acceptance requires a
transactional inbox, explicit request IDs, deduplication, and restart replay
committed consistently with the FSM update. An actor does not itself provide
durability, resource atomicity, or ownership fencing. The package does not
implement that inbox or an actor runtime. The runtime can be developed and
introduced independently, including around the original FSM or a service that
uses no FSM. See [the independent actor/mailbox design](../../docs/actor-mailbox.md)
for package boundaries, cancellation, backpressure, and shutdown semantics.

## Per-flow adoption acceptance criteria

Adopt v2 one protocol/version at a time. Keep legacy records on their original
engine until a reviewed per-flow adapter can map each old state, business record,
history, and uncertain effect to a validated logical v2 snapshot. This mapping
does not itself require rewriting the database. Unsupported data or missing
recovery artifacts require an explicit flow-specific decision. Do not reinterpret
a legacy state name as proof that its old entry action completed.

- Implement the flow-specific `Store` adapter against its existing records and
  history. Prove that loading a snapshot is consistent and the revision check,
  update, history append, and any transactional resource changes are atomic.

- Define durable intents before payment, preimage disclosure, partial-signature
  release, transaction publication, and resource ownership changes. Store exact
  transaction context and safety fallbacks before an irreversible effect.
- Define `Gate`, `Update`, and state `Validate` functions with typed payloads and
  protocol-specific bounds. Validate restored state as strictly as new state.
- Implement `Restore` for every nonterminal state, including initialization,
  cleanup, payment received, signature release, and timeout/retry states. Reconcile
  payment, invoice, chain, and deposit ownership before final success or failure.
- Re-establish chain subscriptions and payment tracking from durable identities.
  Re-evaluate height/time safety margins after downtime; retain absolute durable
  deadlines instead of restarting a full relative timeout.
- Specify single-owner startup/shutdown and worker cancellation. Returning an
  event error must not silently terminate the only worker responsible for a live
  on-chain timeout obligation; the manager must reload/restore or surface an
  actionable failure.
- Test process restart immediately before and after every commit and external
  effect. Inject commit-then-error, rejection, stale revision, unavailable/corrupt
  storage, callback failure/panic, cancellation while queued/running, duplicate
  requests, closed subscriptions, and stale external responses.
- For Instant Out, test recovery before and after HTLC artifact persistence,
  payment initiation, preimage release, and both publication paths. Confirm that
  the preimage never leaves the client without a durable usable fallback and
  sufficient remaining safety margin.
- For static-address Loop In, test lost signature RPC responses, restart from
  each signing boundary, invoice settlement during downtime, partial deposit
  finalization, and timeout sweep rebroadcast. Uncertain signature release must
  preserve monitoring; cleanup must not free a still-encumbered deposit.

The engine provides ordering and recovery contracts. Correct transaction
validation, cryptography, chain/reorg handling, invoice/payment semantics, fee
policy, durable event acceptance, worker supervision, and protocol-specific
fallbacks remain part of the consumer implementation and its review.
