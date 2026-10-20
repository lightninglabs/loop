# Normal client asset deposits and loop-ins

`loopd` starts this manager when tapd and the asset transaction runtime are
configured. It uses the existing static address registration, shared deposit
exclusion, asset payment adapter, durable controller and session worker. The
separate `loopassetd` process owns SDK transaction construction, proofs and
wallet signing. Only versioned primitive data crosses its authenticated Unix
socket; both processes use the same LND and tapd identity.

The first version accepts one whole isolated asset output per deposit and
one deposit per swap. Payout uses the same compressed asset group and the exact
requested native quantity, equal to the deposit minus the agreed fee. The
runtime rejects passive assets, multiple assets in an anchor, and partial
deposit spending. Bitcoin funds on-chain transaction fees; the request
separately caps the server's native asset debit.

## Configure and use

Start `loopassetd` with its private configuration and persistent state
directory. Then supply these options to normal `loopd`, alongside its usual
network, LND and Loop server options:

```text
--tapd.activate
--tapd.host=<tapd-host:port>
--tapd.tlspath=/absolute/path/tapd/tls.cert
--tapd.macaroonpath=/absolute/path/tapd/admin.macaroon
--assetruntimesocket=/absolute/path/runtime/runtime.sock
--assetruntimetoken=/absolute/path/runtime/auth.token
--assetpeer=<compressed asset channel peer public key>
```

All three asset options are required. Startup verifies that the runtime uses
loopd's network and LND identity before restoring asset exclusions and starting
the Bitcoin deposit manager. The Loop server must enable the matching asset
protocol and runtime.

The normal `loop static asset` command group provides:

```sh
loop static asset deposit --group_key=<group> --amount=1000
loop static asset deposits
loop static asset in --group_key=<group> --amount=1000 \
  --utxo=<txid:index> --max_asset_debit=1000 --max_asset_fee=0
loop static asset swaps
loop static asset refund --utxo=<txid:index>
```

These commands reuse `StaticAddressLoopIn`, `ListStaticAddressDeposits` and
`ListStaticAddressSwaps`. Set `asset_params` on a loop-in request, and
`assets_only` on either list request. The response carries `asset_request`,
`asset_deposits` or `asset_requests`; Bitcoin amount fields remain in satoshis
and are not reused for assets. Requests without these fields keep the existing
Bitcoin behavior. Clients must first check `GetInfo.asset_static_loop_in`;
the CLI refuses these variants when an older or disabled daemon omits the
capability, preventing protobuf unknown-field handling from selecting Bitcoin.
An asset loop-in rejects Bitcoin-only options, including
satoshi amounts and fees, instead of ignoring them. `CreateAssetDeposit` and
`RefundAssetDeposit` remain separate operations because they fund a custom
asset anchor and recover its original CSV script.

The deposit and swap commands print a random request ID before sending the RPC.
After an interrupted response, retry the identical command with
`--request_id=<that ID>`. Reusing an ID with different terms is rejected.
Amounts are native asset units; group keys and outpoints use canonical lowercase
hex. Wait for a nonzero deposit confirmation height before starting the swap.

For `in`, `amount` is the exact Lightning payout. The whole selected deposit is
spent, and its excess over that payout must equal the configured server fee and
fit `max_asset_fee`. For example, a 1000-unit deposit with a 10-unit server fee
uses `--amount=990 --max_asset_fee=10`. No change output is created. The separate
`max_asset_debit` bounds the server's outgoing native payment, including
routing, and defaults to the requested payout.

Funding first creates or retrieves the registered static address and stores its
client key locator, client/server public keys and CSV delay. The runtime funds
the asset anchor using those keys. A swap request atomically reserves that
anchor and saves the exact server request bytes before contacting the server.
Subsequent retries preserve the request ID, proof, offer, invoice, quote and
recovery transactions. On the wire, `ServerStaticAddressLoopIn` and
`PushStaticAddressHtlcSigs` carry typed asset payloads with protocol `ASSET_V1`.
The immutable inner offer request remains the digest and restart identity;
Bitcoin fields stay empty. Separate prepare, fallback, status, cancellation
and refund-report operations retain the additional recovery boundaries.

## Recovery and shared ownership

`asset-inventory.db` stores deposits, pending creation requests and their
reservations. `asset-loop-in.db` stores controller progress. Both are private
Bolt databases under the normal network-specific Loop data directory. Keep
these files and the runtime state directory together with the LND/tapd wallet
state. The runtime retains signing sessions and transaction/proof intent; the
client never recreates private keys or nonces during recovery.

On startup, inventory entries are registered with the existing deposit
manager before it recovers Bitcoin deposits. Registration, Bitcoin deposit
creation and selection share the manager mutex. Asset anchors remain excluded
from Bitcoin-only sweeping and withdrawal even after their asset swap or
refund finishes. A conflicting previously stored Bitcoin deposit stops startup.
Inventory release saves a receipt keyed by swap hash, request ID and outpoint.
If a crash interrupts controller cleanup, that receipt lets the old controller
finish without releasing a newer request's reservation.

The client worker resumes pending funding/offer requests and existing sessions.
The shared Loop In and deposit transition policies govern those sessions. Once
claim signatures may have escaped, only local settled payout plus confirmed
claim proof, or a confirmed spendable refund proof, can release the reservation.
The client can discover the execution secret from the chain when the server is
offline. Refund proof reporting retries independently after local success.
Claim proof hints from the server receive strict runtime verification within
a bounded portion of the recovery deadline. Missing, invalid or stale hints
fall back to independent chain discovery with the remaining budget.
Optional server proof and cancellation reports use a one-second timeout so an
unresponsive peer cannot consume a later request's recovery deadline.
The normal server connection also makes waiting for L402 authentication
serialization cancelable for unary calls and stream establishment. A stalled
authenticated request cannot trap another swap's chain-recovery budget.

At the original deposit CSV maturity, unreserved deposits automatically queue
an original-script refund. The `refund` command queues the same deterministic
operation explicitly. A pending offer request or active swap keeps its
reservation, so it cannot race this refund. The runtime independently verifies
maturity, live inputs and asset proofs before signing. Reservations remain held
until a confirmed refund proof is saved atomically with the consumed marker.

A network timeout preserves the request and its reservation. Recovery retries
the exact saved bytes. If an offer expires before any client controller was
created, the manager records cancellation and releases that unstarted request.
If the offer response was lost, it keeps the reservation until the original
CSV maturity. It then checks that no active journal owns the request or anchor,
records cancellation atomically, and permits the original refund. Canceled
requests never restart offer creation. An exposed controller keeps its lock
until its normal proof-gated recovery completes.

If a reorg relocates a refund before invoice cancellation, the controller
replaces its stored proof only after verifying the current spendable refund.
The server report uses that new proof; terminal records remain immutable.
