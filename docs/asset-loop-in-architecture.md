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
| `StaticAddressLoopIn` | `asset_params` contains the durable request ID, group, outpoint and native amounts. Satoshi fields keep their original meaning. |
| `ListStaticAddressDeposits` | `assets_only` selects native deposit records. |
| `ListStaticAddressSwaps` | `assets_only` selects durable asset requests and session status. |
| `ServerStaticAddressLoopIn` | `ASSET_V1` plus typed asset parameters requests the server-owned payment hash. |
| `PushStaticAddressHtlcSigs` | `ASSET_V1` plus the typed asset commitment submits client signatures. |

Mixed Bitcoin and asset options are rejected. The CLI checks the daemon's
asset acknowledgement on each shared method's response. Asset outpoints stay
inside `asset_params`, leaving the Bitcoin selection empty. An older daemon
that ignores these fields rejects creation before starting a Bitcoin swap;
list responses without `assets_only` are rejected. Each operation uses its
existing macaroon permissions.
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

## Ownership in the normal daemons

The SDK is linked into the normal Loop and server modules. There is no asset
companion daemon, transport bridge, private nonce database or transaction
runtime store. The initial separate process existed to isolate incompatible
Bitcoin dependencies; a compatible SDK pin removes that requirement.

| Component | Responsibility |
| --- | --- |
| `swapkit` | Shared Bitcoin deposit/HTLC leaves, control blocks and witnesses, plus typed asset operation data. |
| `swapkit/tapsdk` | Map contracts to SDK transactions, validate asset proofs and policies, and call existing node services. |
| Normal deposit and Loop In managers | Schedule funding, payment, execution and recovery. Their SQL records hold request identity, funding checkpoints, immutable recovery packages and completed fallbacks. |
| LND | Own wallet keys, ephemeral MuSig2 sessions, script signing and chain notifications. |
| tapd | Own transfer logging, publication and confirmed proof reconstruction. |

Unfinished signing attempts restart with fresh LND sessions for the same
unsigned transaction. A completed fallback is saved before claim authority
leaves the client. The server keeps exposure and deposit locks across nonce
rotation and accepts a valid completed transaction from an earlier round.
No private nonce is exported or persisted by Loop.

For a peer's already-mined transaction, the adapter verifies the known package
against the observed witness, registers its OP_TRUE input and logs the transfer
in tapd with broadcasting disabled. It then exports the exact confirmed proof.
This also handles historical v0 assets; a custom proof assembler is unnecessary.

The client SQL store holds asset metadata on `deposits`; one request row holds
admission and execution state. Terminal cleanup, deposit history and reservation
release commit atomically. The server's existing PostgreSQL store shares deposit
locks with Bitcoin. Neither side needs a second asset manager or journal.
Unshipped draft Bolt layouts are not supported upgrade formats.

## Remaining consolidation

The asset execution controller still has mandatory, revision-checked writes
before invoice creation, signature exposure and preimage release. The legacy
FSM's entry notification cannot fail closed on a persistence error. Both
protocols share transition policies and normal managers; merging their final
dispatch layer requires a fallible durable transition barrier and regression
coverage for both protocols.

The compatible SDK pin and small upstream patches are development dependencies.
They must become reviewed release pins. Existing LND chain services replace
custom block scanning; a small Bitcoin RPC callback remains for mempool-aware
UTXO checks on anchors outside LND's wallet inventory.

These priorities supplement the fee, reorg, capacity and operational release
blockers in the [readiness document](asset-loop-in-readiness.md).

The ordinary Loop unit/race jobs now include the contracts and adapter. No
separate module-test workflow is needed.
