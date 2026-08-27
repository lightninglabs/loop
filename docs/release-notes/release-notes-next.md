# Loop Client Release Notes

#### New Features

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.

* Static Address now derives fresh receive and change addresses while retaining
  per-deposit address ownership across restarts for discovery, recovery, and
  signing. `loop static deposit` creates a new address and prints an
  `lncli sendcoins` command for manual funding, with fee selection through
  `--sat_per_vbyte` or `--conf_target`. Deposit listings identify the receiving
  address.
  [PR #1218](https://github.com/lightninglabs/loop/pull/1218)

* Static Address startup reconciles legacy/root, receive, and change key
  counters after an lnd wallet restore. The legacy/root counter also advances
  past every persisted static loop-in HTLC key, so restored wallets don't reuse
  the root key or earlier HTLC keys. Counters behind persisted Loop keys are
  advanced; synchronized or ahead counters are unchanged, with no new addresses
  created. Wallet-watch restoration and key-counter recovery log their progress
  and can be interrupted during startup.

* Static Address funding fees are split across all static-address outputs,
  including deposits to different addresses not yet discovered by loopd.

* Static loop-ins retain original unconfirmed funding transactions so a
  wallet-visible mined replacement can prove the original HTLC cannot confirm
  after restart. Migration 26 adds the funding-transaction snapshots; older
  swaps without them continue monitoring rather than treating missing wallet
  history as proof that deposits can be released.

#### Breaking Changes

* Calling `NewStaticAddress` now derives and returns a fresh receive address
  instead of reusing the address associated with the client's L402.
  Integrations must not assume repeated calls are idempotent or return the
  same address. The RPC now requires the
  `swap:execute` permission instead of `swap:read`, including for address-only
  calls. Operators using custom scoped macaroons must rebake them accordingly.
  The deprecated `StaticAddressSummaryResponse.static_address` field remains
  the legacy/root address for compatibility and must not be treated as the
  current receive address; call `NewStaticAddress` to derive a fresh one.
  The CLI labels this field `legacy_root_address` in `loop static summary`
  output; the RPC field name remains unchanged.
  [PR #1218](https://github.com/lightninglabs/loop/pull/1218)

* The multi-address migration now rejects databases with existing deposits unless
  exactly one legacy static address is available to establish their ownership.
  Its guard uses ordinary schema permissions and does not require PostgreSQL's
  database-level `TEMPORARY` privilege.

  If this migration fails and startup reports `Dirty database version 24`, stop
  loopd and back up the database. Verify that migration 24 rolled back, including
  the absence of the `deposits.static_address_id` column, and correct the cause
  before retrying. For a permission failure in an earlier build, use the
  corrected migration; for ambiguous ownership, seek assistance repairing the
  ownership data without deleting address rows. Only for this verified,
  rolled-back migration 24 failure, reset its migration marker:

  ```sql
  UPDATE schema_migrations SET version = 23, dirty = false
  WHERE version = 24 AND dirty = true;
  ```

  Then restart loopd. Do not apply this reset to another failed migration or a
  database whose schema changes have not been rolled back.

#### Bug Fixes

* Static Address key recovery uses lnd's coin type for each network, including
  coin type 1 on simnet, so synchronized startups consume no additional keys.

* Persist confirmed static withdrawal accounting before finalizing deposits,
  keeping a withdrawal recoverable if change detection or persistence fails.

* Reject static channel opens without confirmed deposits, empty withdrawal
  inputs, and fee rates that overflow conversion before locking or signing.

* Recover static loop-in ownership using persisted swap order, keeping ambiguous
  deposits locked without preventing startup. Recovered owners retain every
  restored input for release and finalization.

* Finish risk-rejected static loop-ins once a confirmed funding conflict makes
  release safe, and retain HTLC monitoring after a reorg while deposits remain
  locked for timeout recovery.

* Completed withdrawal and channel-opening MuSig2 sessions are excluded from
  explicit cleanup, preventing spurious warnings after successful signing.

* Fee bumps of pending Static Address withdrawals reuse their change address
  across restarts, including legacy root-address change.

* Shutdown during standalone startup waits for listener initialization to
  finish, preventing a late Ctrl-C from leaving a REST listener running.

* Static Address deposit timeout sweeps now sign with the deposit key's
  locator. After an lnd wallet restore, root-address timeout sweeps previously
  produced an invalid signature until lnd re-derived the root key.

* Static-address unspent listings refresh before reading wallet outputs and
  exclude inactive historical deposits, preventing stale results after
  replacement, reorg, or spending.

* Static-address issuance waiters now honor context cancellation while root
  creation and address issuance remain serialized and retryable.

* Validate each selected static deposit's CSV expiry against the actual HTLC
  expiry before signing.

* Preserve pending loop-in ownership of deposits during expiry and restart
  recovery. Release only deposits still owned by the swap, after a positive
  unspent-input or confirmed-conflict check rules out a confirmed HTLC.
  Recover abandoned initialization locks at startup, retry temporary wallet
  lookup failures, and finish timed-out swaps after their deposits are reused.
  Check live swap claims before releasing deposits with stale legacy mappings.
  Missing wallet history alone does not release deposits.

* Follow replacement static deposit expiry sweeps after a chain reorganization
  instead of waiting for the old transaction until loopd restarts.

* Static Address loop-in sweep requests are now rejected unless the server
  provides exactly one prevout for every sweep input. A duplicate or missing
  prevout previously crashed `loopd` while computing the sweep signature
  hashes.

* Bound the static Loop In risk decision replay cache to prevent unbounded
  memory growth from swap server notifications for unknown swaps. Cached
  decisions retain only a copy of the validated swap hash, without unknown
  protobuf fields or oversized backing buffers.

#### Maintenance

* Log the candidate swap hashes when startup keeps an ambiguously owned
  static deposit locked, so the conflicting claims can be investigated.

* Static Address startup now avoids reimporting wallet scripts that lnd already
  watches, address lookups remain responsive while new addresses are issued,
  and root creation can recover from a failed wallet import.
  Wallet-import errors are ignored only when they identify the exact script
  that lnd already watches.

- Update the LoopRPC requirement to the revision used by this release.

* Load static addresses in ID-ordered database pages during startup and root
  recovery, publishing the runtime index only after all pages succeed.

* Match wallet UTXOs directly against the active address index under a short
  lock instead of copying every address on each poll.

* Cache the activated legacy static address to avoid scanning all addresses
  during receive and change issuance; failed imports remain retryable.

* Static-address quotes use one exact lookup of active deposits, retaining
  state and expiry checks without scanning or rendering deposit history.

* Static-address listings encode addresses directly from P2TR scripts without
  rebuilding Taproot trees or caching rendered addresses.

* Rename static-address Go APIs to `StaticSingleAddressKeyFamily`,
  `AddressParameters`, and `EnsureStaticAddressRoot`, and consolidate wallet
  UTXO listing into `ListUnspent`. Key-family values remain unchanged.
  [PR #1218](https://github.com/lightninglabs/loop/pull/1218)

#### Contributors (Alphabetical Order)
