# Asset Loop In: review scope and production priorities

This is an experimental implementation for review, not a production release.
The initial flow deposits one isolated asset on chain and pays the same asset
or fungible group over Lightning. The server holds the swap preimage and
controls execution. The client retains unilateral recovery paths.

## What is implemented

- Normal `loop static asset` commands and `loopd` startup/recovery integration.
- Shared deposit and Loop In transition graphs with asset-specific guards.
- Loop-owned deposit/HTLC contracts, an SDK adapter, and the `loopassetd`
  wallet signer/proof runtime. Asset leaves use `OP_TRUE`; spending authority
  remains in the Bitcoin deposit and HTLC scripts.
- Durable request identities, signing sessions, publication records, proof
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
| P0 | Fixed pre-signed fallback fees; no automatic claim/refund fee escalation. | A bounded unilateral fee-bump strategy preserves asset proofs and survives restart. Test fee increases, eviction, and confirmation before deadlines. |
| P0 | Chain-scan reorg tests do not cover every real-node confirmation/cleanup boundary. | Disconnect and replace blocks around deposit, fallback, claim, and refund confirmation with real proofs. Define finality and recovery after terminal cleanup. |
| P0 | Development dependency patches and coordinated protocol changes remain under review. | Review the client/server pair, merge required SDK/tapd fixes, pin released dependencies, and pass required CI, including the process regressions. |
| P1 | Operator visibility and recovery procedures are incomplete. | Expose held-payment age, expiry margin, execution/recovery state, and pending publication; document alerts and stuck-swap procedures without leaking secrets. |
| P1 | Per-swap caps do not bound aggregate exposure. | Enforce and test aggregate/concurrent limits against channel and wallet capacity across owners and repeated failed requests. |
| P1 | Single-server process restart is not clustered failover or disaster recovery. | Exercise leadership handoff, runtime single-writer ownership, backup restore, upgrades with pending swaps, and full L402 token acquisition. |

P0 blocks any production rollout. P1 must also be completed before an
unrestricted service rollout. No mainnet readiness is implied by the draft PR.

Cooperative direct sweep, partial/multiple deposits, passive assets, and
cross-group payouts are outside the initial scope. Direct sweep is an
optimization; reliable unilateral recovery is a requirement.

## Dependencies and local checks

Taproot Assets' hold-invoice CLTV forwarding fix is merged. Native invoice
rounding still needs an upstream change. The generic SDK requirements are
[anchor outpoint metadata](https://github.com/lightninglabs/tap-sdk/pull/187)
and [custom-anchor signing metadata](https://github.com/lightninglabs/tap-sdk/pull/188).
Both SDK PRs passed CI at the October 20, 2026 review; that does not substitute
for integration checks on this branch.

Follow the [SDK development setup](../swapkit/tapsdk/README.md#development-setup)
before building the separate runtime. The root Loop module deliberately keeps
its older dependency graph and communicates with that runtime through the
primitive bridge protocol.

```sh
go test ./staticaddr/... ./assets/assetln ./loopd ./cmd/loop
(cd swapkit && go test ./...)
(cd swapkit/tapsdk && go test -race ./runtime ./walletsigner)
```

See the [asset state mapping](../staticaddr/loopin/asset-fsm.md),
[client manager](../staticaddr/loopin/assetmanager/README.md),
[execution controller](../staticaddr/loopin/assetexec/README.md), and
[runtime](../swapkit/tapsdk/cmd/loopassetd/README.md) for ownership and restart
requirements. Keep this document updated as each release blocker is closed.
