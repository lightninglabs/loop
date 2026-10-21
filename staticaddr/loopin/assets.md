# Asset deposits and Loop In

Asset deposits and swaps run in the existing deposit and static Loop In FSMs.
They use the ordinary SQL records, manager admission and startup recovery.
Asset metadata contains native quantities, the asset contract and proofs;
Bitcoin amount fields continue to contain only the anchor's satoshis.

The first version accepts one whole isolated asset output per deposit and one
per swap. The Lightning payout uses the same asset group and exact native
quantity. The difference between the deposit and payout is the agreed asset
fee. Partial deposits, change, passive assets and multipart payments are not
supported initially. Bitcoin pays the anchor transaction fees.

## Configuration and commands

Supply the ordinary `loopd` network, LND and server configuration together with:

```text
--tapd.activate
--tapd.host=<tapd-host:port>
--tapd.tlspath=/absolute/path/tapd/tls.cert
--tapd.macaroonpath=/absolute/path/tapd/admin.macaroon
--assetpeer=<compressed asset channel peer public key>
--assetbitcoinurl=<Bitcoin RPC URL>
--assetbitcoinuser=<Bitcoin RPC user>
--assetbitcoinpassword=<Bitcoin RPC password>
```

`assetbitcointls` supplies a private Bitcoin RPC CA. This connection checks
current UTXO and mempool spendability; normal LND services supply blocks and
spend notifications. Anchor fees default to 1000 satoshis, with a 2000-satoshi
funding cap (`assetfeesat` and `assetmaxfeesat`). Startup checks the tapd network
and signing-node identity before normal recovery starts.

```sh
loop static asset deposit --group_key=<group> --amount=1000
loop static asset deposits
loop static asset in --group_key=<group> --amount=990 \
  --utxo=<txid:index> --max_asset_debit=990 --max_asset_fee=10
loop static asset swaps
loop static asset refund --utxo=<txid:index>
```

The example swaps a 1000-unit deposit for a 990-unit Lightning payout and a
10-unit service fee. `max_asset_debit` separately caps the server's native
outgoing payment, including routing. Identifiers must use canonical lowercase
hex. Wait for a confirmed deposit before starting a swap.

Creation prints a request ID. A funding request uses that ID as its ordinary
deposit ID. A swap stores it alongside its normal payment hash; replaying the
same request returns that swap. An initial attempt interrupted before claim
exposure is abandoned by normal recovery. A new attempt needs a new request
ID. The implementation does not promise to resume every interrupted RPC.

These commands reuse `StaticAddressLoopIn`, `ListStaticAddressDeposits` and
`ListStaticAddressSwaps`: `asset_params` selects native swaps, and `assets_only`
selects native status records. The response contains `asset_request`,
`asset_deposits` or `asset_requests`. The legacy Bitcoin status stream has no
native quantity fields, so native status is available in the static lists.
Bitcoin request fields remain empty for asset operations; older daemons reject
an asset request they do not understand. The CLI checks the asset response
acknowledgement. Asset requests reject unrelated Bitcoin-only options.
`CreateAssetDeposit` funds an asset anchor and `RefundAssetDeposit` starts its
original CSV expiry action.

## Ownership and recovery

The SDK kit lives in Loop and uses the normal LND signer and chain services.
Tapd publishes transfers and constructs confirmed proofs. There is no
companion process, private signer database or asset execution journal.

The deposit manager stores the final signed funding package on the normal
`Deposit` before publication. Its FSM publishes and confirms it, then enters
`Deposited`. Proof-bearing deposits are excluded from Bitcoin selection and
Bitcoin-only spending actions. Default deposit lists, unspent lists and static
address summaries show only Bitcoin deposits; use `assets_only` for native
inventory. Asset deposits' original CSV refund runs in the same expiry states,
using an asset transaction and proof operation instead.

`StaticAddressLoopIn.Asset` carries the server offer, HOLD invoice, unsigned
recovery package, complete fallback and accepted asset HTLC evidence. The
ordinary swap state describes its progress. The client does not initially know
the server's preimage; the normal SQL preimage field is nullable until learned.

`InitHtlcTx` obtains the server hash, creates the ordinary swap row and
reserves its deposit. `SignHtlcTx` creates the same-group HOLD invoice,
prepares the asset fallback and verifies local native acceptance. It explicitly
saves the completed fallback and monitoring state before sending claim
signatures. MuSig nonces remain ephemeral in LND; a new attempt uses new
sessions. Initial states recover by safely aborting, as Bitcoin swaps do.

After exposure, `MonitorInvoiceAndHtlcTx` retries the conditional signature
submission only while the same local HTLC and expiry remain valid. Server
preimage disclosure permits local settlement. The ordinary `PaymentReceived`
action finalizes the deposit immediately after verified local settlement; the
client does not track the server's claim proof for completion.

On timeout, the normal swap FSM publishes its saved fallback and asset refund.
A confirmed spendable refund permits invoice cancellation and deposit
finalization. An unavailable server cannot veto that local path. Reporting the
refund proof is best effort. Original deposits locked in `LoopingIn` stay
under the swap FSM's recovery rather than running a competing CSV refund.

Back up the normal Loop database and corresponding LND/tapd state. Earlier
unshipped controller/request journals are not an upgrade format. See the
[state mapping](asset-fsm.md) and [production priorities](../../docs/asset-loop-in-readiness.md)
for the remaining release requirements.
