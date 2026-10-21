# Asset Loop In: review scope and production priorities

This is an experimental implementation for review, not a production release.
The initial flow deposits one isolated asset on chain and pays the same asset
or fungible group over Lightning. The server holds the swap preimage and
controls execution. The client retains unilateral recovery paths.

See the [architecture review](asset-loop-in-architecture.md) for RPC reuse,
the normal daemon integration, and the remaining consolidation priorities.

## What is implemented

- Normal `loop static asset` commands and `loopd` startup/recovery integration.
- Normal managers and SQL connections host a separate asset controller,
  request workflow and recovery batches. Actual deposit/Loop In FSM reuse is
  still missing; shared graphs and database objects do not provide it.
- Loop-owned deposit/HTLC contracts and an SDK adapter using normal LND signing
  and tapd proof publication. Asset leaves use `OP_TRUE`; spending authority
  remains in the Bitcoin deposit and HTLC scripts.
- Durable request identities, completed fallbacks, tapd publication records, proof
  import, deposit exclusion, and recovery after process restarts.
- Same-group native asset HOLD invoices, accepted-HTLC amount/group/expiry
  validation, and server-controlled preimage release.

The companion server process harness has exercised the normal CLI/daemon flow
on regtest, including Lightning settlement before fallback confirmation,
lost signing responses, restart during publication, and original-deposit
recovery after a long outage. This is evidence for those scenarios, not a
claim of comprehensive failure or deployment coverage.

## Priorities before production

| Priority | Shortcoming | Completion criterion |
| --- | --- | --- |
| P0 | Parallel asset lifecycle instead of existing FSM reuse. | Follow the [architecture audit](asset-loop-in-architecture.md): use normal deposit/swap records, FSM actions and recovery; remove the separate controllers, request workflow and revision framework. |
| P0 | Fixed pre-signed fallback fees; no automatic claim/refund fee escalation. | A bounded unilateral fee-bump strategy preserves asset proofs and survives restart. Test fee increases, eviction, and confirmation before deadlines. |
| P0 | Chain-notification reorg tests do not cover every real-node confirmation/cleanup boundary. | Disconnect and replace blocks around deposit, fallback, claim, and refund confirmation with real proofs. Define finality and recovery after terminal cleanup. |
| P0 | Development dependency patches and coordinated protocol changes remain under review. | Review the client/server pair, merge required SDK/tapd fixes, pin released dependencies, and pass required CI, including the process regressions. |
| P1 | Operator visibility and recovery procedures are incomplete. | Expose held-payment age, expiry margin, execution/recovery state, and pending publication; document alerts and stuck-swap procedures without leaking secrets. |
| P1 | Per-swap caps do not bound aggregate exposure. | Enforce and test aggregate/concurrent limits against channel and wallet capacity across owners and repeated failed requests. |
| P1 | Single-server process restart is not clustered failover or disaster recovery. | Exercise leadership handoff,  backup restore, upgrades with pending swaps, and full L402 token acquisition. |

P0 blocks any production rollout. P1 must also be completed before an
unrestricted service rollout. No mainnet readiness is implied by the draft PR.

Cooperative direct sweep, partial/multiple deposits, passive assets, and
cross-group payouts are outside the initial scope. Direct sweep is an
optimization; reliable unilateral recovery is a requirement.

## Dependencies and local checks

The normal Loop and server modules now import the SDK directly. Development
setup pins the compatible SDK base `932b4aa5429b66df2df13fcfac03fc6c6bdf974e`
and Taproot Assets v0.8.6. The small SDK compatibility patch includes generic
metadata fixes, compressed participant handling and current proof rules.
The stable tapd checkout needs the merged CLTV fix backported and the existing
native amount rounding fix. These pins and patches remain release blockers.

Run the package checks from the normal modules; there is no separate runtime
build. The server's `make asset-loop-in-production-itest` exercises normal
client/server processes and `make asset-loop-in-itest` covers protocol cases.
The production priorities above still apply after the architecture reduction.
