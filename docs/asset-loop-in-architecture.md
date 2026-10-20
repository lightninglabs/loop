# Asset Loop In architecture and refactoring priorities

The first draft proved the full asset flow and restart scenarios, but introduced
too many parallel interfaces and lifecycle owners. Sharing state names and
transition graphs is not the same as sharing the existing managers, stores,
and transaction execution. This document distinguishes the necessary asset
work from the compatibility scaffolding and tracks the remaining reductions.

## RPC reuse

The public API now extends the existing methods with typed asset fields:

| Existing method | Asset variant |
| --- | --- |
| `StaticAddressLoopIn` | `asset_params` contains the durable request ID, group and native amounts. Satoshi fields keep their original meaning. |
| `ListStaticAddressDeposits` | `assets_only` selects native deposit records. |
| `ListStaticAddressSwaps` | `assets_only` selects durable asset requests and session status. |
| `ServerStaticAddressLoopIn` | `ASSET_V1` plus typed asset parameters requests the server-owned payment hash. |
| `PushStaticAddressHtlcSigs` | `ASSET_V1` plus the typed asset commitment submits client signatures. |

Mixed Bitcoin and asset options are rejected. The CLI checks the daemon's
`GetInfo.asset_static_loop_in` capability before using the shared methods.
The server protocol version and empty Bitcoin fields make old servers reject
asset requests. Inner asset request bytes remain unchanged, preserving durable
request digests and retries.

Creating an asset deposit actively funds a transaction; requesting a Bitcoin
static address does not. Original CSV recovery also differs from a cooperative
withdrawal. Those operations retain distinct methods.

The server still needs a separate preparation exchange: it must choose the
hash before the client creates its HOLD invoice. Fetching recovery signatures,
observing execution, cancellation, and reporting a confirmed refund are
separate durable operations. Reusing method names must not make the client
able to execute the swap or skip these persistence boundaries.

## What the kit and runtime actually contain

At the first draft, the `swapkit` tree contained roughly 5,700 lines of
production Go, 4,500 lines of tests, 2,300 lines of dependency metadata, and
fixtures/documentation. The small contract kit was about 500 production lines;
the SDK adapter about 300. The rest was not a contract kit:

| Component | Responsibility | Assessment |
| --- | --- | --- |
| Contract kit | Bitcoin deposit/HTLC scripts, control blocks and witnesses. | Keep a small shared implementation. Existing Bitcoin helpers and asset contracts now use the same leaf serializers. |
| SDK adapter | Convert contract outputs and signing plans into generic SDK requests. | Keep thin; generic asset construction belongs in the SDK. |
| Runtime, about 2,900 lines | Asset inventory, fallback/claim/refund construction, proof import/reconstruction, chain discovery and transaction journal. | Necessary operations, but too many responsibilities and duplicated lifecycle ownership in one package. |
| Wallet signer, about 900 lines | Wallet key authorization and durable MuSig/signature recovery. | Preserve nonce/transaction bindings and restart guarantees; do not replace with ephemeral sessions. |
| Bridge and daemon, about 1,100 lines | Authenticated process transport, configuration and SDK hosting. | Compatibility cost, not a requirement of asset swaps. Remove when dependencies can coexist. |

The first refactoring removes duplicate contract/VM tests from the SDK adapter
and keeps them in the core kit. It also shares unilateral spend planning and
the commit/persist/sign/persist sequence between original refunds and HTLC
recovery. These changes remove duplication; they do not resolve the larger
lifecycle design problem.

## Why a separate process exists today

The current root already uses LND 0.21. The obstacle is not simply an old LND
version. Loop uses classic btcd/wallet types while the SDK graph uses split
`wire/v2`, `txscript/v2`, PSBT and newer wallet types. An isolated mixed-module
compile was tested on October 20, 2026. Pinning classic btcd made module
resolution succeed, but compilation failed in wallet/neutrino/LND dependencies
on incompatible types. Pinning those dependencies to Loop's versions moved the
same mismatches into the SDK's anchor builder.

An in-process driver is the preferred target. Getting there requires a
coordinated dependency migration or an SDK compatibility implementation;
removing the process boundary without that work does not produce a buildable
client/server. The independent process must not become a permanent requirement
merely because the first integration used it.

## Remaining architecture priorities

1. **Shared lifecycle and persistence.** Extend the normal deposit and Loop In
   records with asset metadata and driver hooks, then remove parallel inventory
   and execution journals. First give the shared FSM a fallible durable barrier
   before external effects; its existing notification hook cannot substitute
   for the current fail-closed writes. Keep all lost-response and restart tests.
2. **In-process SDK driver.** Align the dependency graphs, remove the JSON
   bridge and extra daemon, and use the normal daemon configuration/lifecycle.
   Track this independently from changes to contract authority.
3. **Existing chain services.** Reuse LND confirmation/spend subscriptions,
   height hints and ChainKit retrieval where they replace custom scanning.
   Preserve current-chain proof validation and the arbitrary-outpoint mempool
   unspent check. A notification timeout or wallet `ListUnspent` is not evidence
   that an externally controlled anchor is unspent.
4. **Proof API reuse.** Move generic confirmation/proof work into the SDK once
   it covers the required historical asset versions. Its current helper cannot
   directly replace the tested v0 proof path.

These architecture priorities supplement the fee, reorg, capacity and
operational release blockers in the [readiness document](asset-loop-in-readiness.md).
A smaller diff must preserve server execution authority, native asset
validation, and recovery after either process loses a response or restarts.
