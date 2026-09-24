# Loop Client Release Notes

#### New Features

* `SuggestSwaps` (`loop suggestswaps`) now works as a dry run for easy
  autoloop. When easy autoloop is enabled it returns the Loop Out that easy
  autoloop would make next, or sets the new `easy_autoloop_reason` field to the
  reason that it would not make one. Two reasons were added for it:
  `AUTO_REASON_BELOW_MINIMUM` and `AUTO_REASON_NO_ELIGIBLE_CHANNEL`. Before,
  the call ignored easy autoloop and failed when no rules were set. A budget
  that is not spent yet, but that the worst-case fees held for swaps in flight
  fill, is reported as `AUTO_REASON_BUDGET_INSUFFICIENT` rather than
  `AUTO_REASON_BUDGET_ELAPSED`, since there may be room again once those swaps
  finish.

* With easy autoloop enabled, `SuggestSwaps` lists the channels that easy
  autoloop skipped in `disqualified`, with the reason for each. Two reasons
  were added for it: `AUTO_REASON_CHANNEL_INACTIVE` and
  `AUTO_REASON_PEER_EXCLUDED`.

* Static addresses now support local labels that can be set at creation,
  updated or cleared through the CLI and RPC, and viewed per address in
  unspent deposit listings. Deposits in deposit, withdrawal and loop-in
  listings report both the receiving address's label and the lnd label of
  their funding transaction. Address labels are independent of funding
  transaction labels. Change addresses created by withdrawals and loop-ins
  inherit the distinct labels of the spent deposits' addresses, joined with
  commas up to the label length limit.
  [PR #1178](https://github.com/lightninglabs/loop/pull/1178)

#### Breaking Changes

#### Bug Fixes

* Fix a deadlock in the static address loop-in, withdrawal and open channel
  managers. If the caller of a request stopped waiting before the manager
  answered, for example because the `loop` command was interrupted, the
  manager blocked on its response and served no further requests of that
  kind until `loopd` was restarted.

* Fix a hang in the recovery of unfinished loop outs. If the server reported
  two unfinished swaps in quick succession, the recovery stopped for good
  until `loopd` was restarted. A recovery attempt that is retried after a
  failure also no longer outlives `loopd`'s shutdown.

* SQLite write transactions now take the database write lock when they begin.
  Before, a transaction that read before it wrote failed at once with
  `database is locked (SQLITE_BUSY)` if another write was in progress, instead
  of waiting for the busy timeout. For example, loopd could log this error when
  it stored the server's confirmation risk decision for a static Loop In.

* A static address loop-in no longer takes a confirmed transaction that pays to
  its htlc as the htlc transaction unless that transaction also spends the
  swap's deposits. Before, the server could confirm a dust output to the htlc
  script and lock the deposits in a timeout sweep that could never succeed.
  The htlc transaction is then found from the spend of the deposits and its
  confirmation is taken by transaction id.

* Automatic Loop Out recovery sweeps and `loop out sweephtlc` in database
  mode now wait for the swap's stored `htlc_confs` confirmations before
  signing the sweep. Before, they waited for only one confirmation. Automatic
  recovery sweeps also no longer reveal the preimage when 20 blocks or fewer
  are left before the HTLC expires, or when the HTLC holds less than the swap
  amount, unless the swap already revealed it or completed. Extra coins sent
  to the HTLC address of a completed swap are still recovered at any amount
  and height.

#### Maintenance

* Update the OpenTelemetry Go SDK from v1.45.0 to v1.47.0 in Loop and
  `looprpc`.

#### Contributors (Alphabetical Order)
