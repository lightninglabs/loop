# Asset Loop In: review scope and production priorities

This is an experimental implementation for review, not a production release.
The initial flow deposits one isolated asset on chain and pays the same asset
or fungible group over Lightning. The server holds the swap preimage and
controls execution. The client retains unilateral recovery paths.

See the [architecture review](asset-loop-in-architecture.md) for RPC reuse,
the normal FSM ownership, protocol saves, and restart boundaries.

## What is implemented

- Normal `loop static asset` commands and `loopd` startup/recovery integration.
- Ordinary deposit and static Loop In FSMs, records and startup recovery.
  Asset metadata selects native payment/transaction actions; no separate
  controller, request journal or recovery worker remains.
- Native SDK Build/Commit calls in the ordinary FSM actions, with existing LND
  signing and tapd publication. Loop-owned helpers enforce deposit/HTLC policy,
  current-chain checks and observed-spend proof recovery. Asset leaves use
  `OP_TRUE`; Bitcoin deposit and HTLC scripts enforce spending authority.
- Complete signed packages and fallbacks saved before publication or claim
  exposure, tapd proof import, and recovery through the ordinary stores.
- Same-group native asset HOLD invoices through existing LND/tapd clients,
  accepted-HTLC amount/group/expiry checks, and server-controlled preimage
  release. The normal invoice field and one finalized fallback package retain
  the client data; the server saves the generated payment request before send.

The companion server process suite exercises the normal CLI/daemon flow on
regtest: server-held execution, restart during publication, unilateral refunds
and force-close recovery. Early uncommitted handshakes may abort safely. These
tests do not establish comprehensive failure or deployment coverage.

## Priorities before production

| Priority | Shortcoming | Completion criterion |
| --- | --- | --- |
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
and Taproot Assets v0.8.6. The SDK compatibility patch includes generic
metadata fixes, compressed participant handling and current proof rules.
The stable tapd checkout needs the merged CLTV fix backported and the existing
native amount rounding fix. These pins and patches remain release blockers.

Run the package checks from the normal modules; there is no separate runtime
build. The server's `make asset-loop-in-production-itest` exercises normal
client/server processes; `make asset-loop-in-itest` is an alias for that suite.
Unit tests exercise native circuit checks, saved SQL phases, failed critical
writes and malformed package rejection. Successful signing, proof publication
and unilateral recovery are checked by the normal-process suite. Earlier PR
databases using transaction/payment wrappers or duplicate fallback fields must
be recreated; recovery tests cover the current format. The production
priorities above still apply after the architecture reduction.
