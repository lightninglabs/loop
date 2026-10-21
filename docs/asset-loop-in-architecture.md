# Asset Loop In architecture audit

The asset implementation still runs a second workflow inside the normal
managers. It does not yet reuse the deposit and Loop In FSMs as requested.
The `assetexec` controllers, separate recovery workers and lifecycle journals
should be removed. Asset operations belong in the existing FSM actions and
records. Moving the controllers into another package would not fix this.

This audit compares Loop `48d8019` with its PR base `ba908ea`, and server
`9e27ff31` with its PR base `0cfde3f3`, on October 21, 2026. It separates
protocol requirements from extra durability introduced by the implementation.
The target below is a refactoring plan, not a claim that integration is done.

## What the existing flow already does

The client creates one `loopin.FSM` per swap, serializes actions through
`fsm.StateMachine.SendEvent`, saves the swap and state history, and restores
pending swaps through `recoverLoopIns` and `OnRecover`. The deposit manager
similarly owns deposit FSMs, expiry, swap ownership and terminal states.
Initial client signing states can recover by aborting and unlocking; normal
Loop does not promise to resume every interrupted admission RPC.

The server already has synchronous `persistLoopInState` calls before payment
and deposit release in `staticaddr/loopin/actions.go`. It already stores
preimages and signed HTLC transactions, restores pending FSMs, tracks payments
by hash, and shares owner-scoped deposit locks with withdrawals. These
facilities exist in the PR base; this asset change did not introduce them.

The earlier documentation argued that a separate engine was necessary because
the FSM entry callback cannot return a persistence error. That conclusion was
wrong. Critical asset actions can explicitly save their state and stop on
failure, following the existing server payment action. There is no need to
redesign the generic FSM or introduce revision checks for every transition.

## Where reuse currently stops

| Area | Current implementation | Required correction |
| --- | --- | --- |
| Client swap execution | `assets.go` creates `assetclient.Session` and `assetexec.Controller`; normal `NewFSM` never receives an asset swap. | Select asset actions inside the existing Loop In FSM. |
| Deposit execution | `RegisterAssetDeposit` adds an exclusion-map entry. Ordinary queries exclude asset rows; asset code manually writes deposit history. | Load asset metadata into ordinary `Deposit` objects and use their FSMs. |
| Server execution | `NewFSM` accepts only V0. `assetexec.Engine` executes the asset protocol. | Add the asset protocol to the existing FSM and active-swap registry. |
| Recovery | Managers host extra request/session scans, timers and bounded worker batches. | Restore asset FSMs through ordinary startup recovery and event handling. |
| Transition maps | `flow` selects separate Bitcoin and asset graphs; it shares names, not actions. | Keep the ordinary FSM maps and select the few actions/transitions that differ. |
| Storage | The client stores both `AssetJob` and `assetexec.Record`; the server has `asset_loopin_executions`. | Extend normal swap/deposit records. An asset-data side table is reasonable; a second lifecycle journal is not. |

Before the small dead-code cuts accompanying this audit, the client controller,
session, asset store/actions and graph packages contained 3,550 production Go
lines and 3,514 test lines. The corresponding server engine, service, payment,
RPC, graph and asset-store files contained 3,346 production lines and 2,542 test
lines. These counts exclude the SDK adapter and generated bindings. They are
not deletion estimates: the packages also contain required protocol checks.

`swapkit/tapsdk` has another 3,008 production lines. Some map asset transactions
and proofs to the SDK; others implement the additional retry/checkpoint model.
The large diff therefore cannot be explained simply as necessary SDK code.

## Keep the asset protocol; remove the extra machinery

| Keep | Put it here |
| --- | --- |
| Asset/group identity, native quantities, input proofs, wallet locator and anchor policy | Typed optional data on `Deposit` and `StaticAddressLoopIn`; native units remain separate from anchor satoshis. |
| Server-generated preimage and server-only execution decision | Server swap record and explicit Loop In action/state. No client execute RPC. |
| Same-group HOLD invoice, RFQ and actual accepted-HTLC quantity/expiry checks | Asset branches of existing payment and invoice actions, using the native payment adapter. |
| OP_TRUE virtual asset transactions and existing Bitcoin deposit/HTLC scripts | Loop-owned kit calling the SDK. LND owns signing; tapd owns publication/proofs. |
| Completed fallback available before client claim authority leaves | Checked save in the signing action using the normal swap store. |
| Payment tracking and claim/refund data needed after restart | Existing swap record and normal recovery actions. |
| Original CSV refund and asset HTLC refund | Existing deposit expiry and Loop In timeout actions with asset transaction/proof handling. |
| No unlocking while an exposed claim can still execute | Explicit recovery/error transitions and existing owner-scoped locks. |

Remove the second executor, striped locks, revision compare-and-swap protocol,
whole-record immutable comparisons, duplicate transition validation and mirrored
deposit state. Represent protocol progress in the FSM and validate untrusted
inputs at the boundary or before the relevant effect.

Remove the durable `AssetJob` workflow, reporting outbox flags, separate cleanup
receipts and repeated active-record scans as part of normal-row integration.
Funding, offer and invoice RPCs need not all resume transparently. Before any
payment or signature can escape, a failed attempt may abort and release its
reservation using the ordinary flow. Once an effect may exist, recovery must
inspect the original payment/transaction rather than assume failure.

The SDK funding API currently checkpoints a plan, an unsigned package and a
final package through a callback. Keeping a completed package before publication
serves restart recovery. Saving every intermediate preparation step is an extra
retry guarantee. Move persistence into the deposit action and let incomplete
preparation restart with fresh wallet sessions and proper wallet cleanup.

The server driver also reloads the swap solely to copy `assetexec` structures
into matching `swapkit` structures. Pass the FSM's typed asset data directly to
narrow kit methods. The kit should not orchestrate database writes or retries.

## State/action mapping

| Existing phase | Asset adaptation |
| --- | --- |
| Deposit discovery and `Deposited` | Attach verified native metadata; exclude asset anchors from Bitcoin-only operations through typed dispatch. |
| Deposit `PublishExpirySweep` / `WaitForExpirySweep` | Build the CSV asset refund and obtain its proof with the kit/tapd. |
| Client `InitHtlcTx` | Obtain the server-owned hash and create its matching same-group HOLD invoice. |
| Client `SignHtlcTx` | Verify accepted native payment, finalize the fallback and save it before sending claim authority. |
| Client `MonitorInvoiceAndHtlcTx` | Observe the local invoice and server/on-chain preimage; settle or enter timeout recovery. |
| Client `PaymentReceived` | Finish on verified local settlement, as normal Loop does. Server claim confirmation belongs to the server. |
| Client timeout states | Publish and monitor the asset fallback/refund using the saved package. |
| Server initialization/payment phases | Validate the deposit, retain its secret and start/track the native payment by hash. Accepted HOLD payment must precede client claim signatures. |
| Server finalize/execution phase | Verify completed fallback; wait for trusted server execution policy; save before disclosing the secret. Add an explicit wait/execute state where existing states do not express this. |
| Server publish/monitor/resolution phases | Use asset-aware transaction/proof actions and ordinary payment tracking. Retain locks while claim or payout outcome is unresolved. |

This mapping does not force accepted HOLD payment into `InvoicePaid`. It is
still unsettled. Claim publication must also proceed when the client is offline
and the payout has not settled yet. Asset signing recovery cannot inherit a
Bitcoin `OnRecover` edge that unlocks potentially exposed deposits. These
ordering differences justify a few new states and error/recovery edges inside
the existing FSM.

Normal record reuse needs deliberate schema work. In particular, the client
`swaps.preimage` is `NOT NULL UNIQUE`; server-owned-preimage swaps need an
explicit unknown value until disclosure, not a zero preimage placeholder.
Existing SQL mapping also assumes some Bitcoin-only keys/addresses are present.
Adapt those assumptions alongside optional asset metadata and regenerate SQL.

## Removal order and validation

1. Completed in this pass: remove the unused client cooperative-signature
   method, pending-only signer stub, and server no-op preimage-notification
   hook with their fields and tests. Real disclosure remains status polling
   or a claim witness. The remaining steps are still outstanding.
2. Put asset deposits into the real deposit FSM/store. Replace mirrored state,
   request ownership and consumed flags with normal state/ownership fields.
3. Extend ordinary swap records and migrate the client signing, invoice and
   timeout actions. Remove `assetexec`, `assetclient.Session`, the request
   workflow and the extra recovery batches.
4. Extend the server FSM/registry and normal swap store. Move native payment
   checks beside existing payment actions; remove `assetexec`, `assetservice`
   lifecycle plumbing, the separate journal and the graph facade.
5. Narrow the kit API after its callers use normal swap data. Keep transaction,
   signature, proof and native-payment tests; delete tests whose sole purpose
   is preserving the removed revision/worker/outbox abstractions.

The refactor must pass normal Bitcoin regression tests and asset FSM/process
checks for server-held execution, restart with a completed fallback, restart
around payment/disclosure/publication, and unilateral HTLC/original-deposit
refunds. A restart before commitment may abort safely. Success means those
scenarios run through `NewFSM`, ordinary recovery and the normal stores; an
unchanged controller behind a new wrapper does not meet that criterion.

Fee escalation, real-node reorg coverage and dependency review remain tracked
in the [readiness document](asset-loop-in-readiness.md). They do not justify
retaining the parallel lifecycle or expanding this change into a general
Loop durability overhaul.
