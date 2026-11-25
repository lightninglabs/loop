# Loop Client Release Notes

- **Release date:** Pending
- **Release page:** [v0.36.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.36.0-beta)
- **Previous release:** [v0.35.4-beta](release-notes-0.35.4.md)
- **Next release:** [Next release](release-notes-next.md)

#### New Features

* Static Address now derives fresh receive and change addresses while retaining
  per-deposit address ownership across restarts for discovery, recovery, and
  signing. `loop static deposit` creates a new address and prints an
  `lncli sendcoins` command for manual funding, with fee selection through
  `--sat_per_vbyte` or `--conf_target`. Deposit listings identify the receiving
  address. Existing receive addresses remain usable. Automation that sends to
  a saved address can keep using it, while change can arrive at a different
  address owned by the same client.
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

* `loop static openchannel` now defaults to anchor channels. Remove
  `--channel_type=tweakless` from scripts to use the new default. RPC callers
  that specify `STATIC_REMOTE_KEY` must omit the commitment type or select a
  supported type. Opening a public anchor channel requires an on-chain
  reserve in the funding node's lnd wallet.
  [PR #1306](https://github.com/lightninglabs/loop/pull/1306)

* Calling `NewStaticAddress` now derives and returns a fresh receive address
  instead of reusing the address associated with the client's L402.
  Integrations must not assume repeated calls are idempotent or return the
  same address. The RPC now requires the
  `swap:execute` permission instead of `swap:read`, including for address-only
  calls. The RPC also requires `loop:in`. Operators using permission-scoped
  macaroons should check that they grant both operations.
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

* Reject static Loop In server responses with missing or extra HTLC nonces
  before signing, preventing a client crash from an incomplete nonce list.
  [PR #1300](https://github.com/lightninglabs/loop/pull/1300)

* A static Loop In that releases its deposits a second time after its payment
  deadline, at its HTLC expiry or when the server rejects the swap, now only
  releases deposits that are still looping in for it. Before, it could move a
  deposit that another Loop In, a withdrawal or a channel open had taken in the
  meantime back to `Deposited`, and retried forever once the deposit was spent.
  A static Loop In also no longer locks a deposit that another Loop In is
  already using, and a failed lock no longer resets deposits that a withdrawal
  or a channel open has taken.
  [PR #1279](https://github.com/lightninglabs/loop/pull/1279)

* A static Loop In no longer releases its deposits when pushing its HTLC
  signatures to the server fails or when loopd restarts during signing. The
  server may have received the signatures and can publish the HTLC, so the
  client continues monitoring for confirmation or payment. At the payment
  deadline or HTLC expiry, release depends on deposit ownership and wallet
  evidence. If the outcome is uncertain, monitoring can continue past expiry.
  [PR #1291](https://github.com/lightninglabs/loop/pull/1291)

* The static address Loop In timeout sweep now spends the exact HTLC output
  confirmed on chain, including server-published backup fee variants.
  Previously it always targeted the standard fee variant, so a confirmed high
  fee or extremely high fee HTLC transaction could not be swept back after the
  HTLC timed out. Sweeps that would leave a dust output are no longer signed.
  [PR #1269](https://github.com/lightninglabs/loop/pull/1269)

* Fractional static loop-ins defer change-address persistence and wallet imports
  until the server response and local checks succeed, avoiding unused address
  rows and watches on rejected requests or excessive fees.

* Static Address key recovery uses lnd's coin type for each network, including
  coin type 1 on simnet, so synchronized startups consume no additional keys.

* Persist confirmed static withdrawal accounting before finalizing deposits.
  Other persistence failures, for example a busy database, are retried with a
  growing delay until finalization succeeds, and a withdrawal that is still
  pending at shutdown is recovered on restart; missing history still allows
  confirmed deposits to finalize, with a warning.

* Reject static channel opens without confirmed deposits, empty withdrawal
  inputs, and fee rates that overflow conversion before locking or signing.

* Recover static loop-in ownership using persisted swap order, keeping ambiguous
  deposits locked without preventing startup. Recovered owners retain every
  restored input for release and finalization. Cooperative change validation
  rejects incomplete original-input metadata.

* Finish risk-rejected static loop-ins once a confirmed funding conflict makes
  release safe, and retain HTLC monitoring after a reorg while deposits remain
  locked for timeout recovery.

* Preserve legacy root-address change accounting for static withdrawals that
  confirm after upgrading to multi-address support, including withdrawals whose
  destination is the root address.

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

#### Maintenance

* The regtest guide documents how to refresh the Loop server image and
  configure Bitcoin RPC and TLS when running the server separately.
  [PR #1299](https://github.com/lightninglabs/loop/pull/1299)

* Log the candidate swap hashes when startup keeps an ambiguously owned
  static deposit locked, so the conflicting claims can be investigated.

* Static Address startup now avoids reimporting wallet scripts that lnd already
  watches, address lookups remain responsive while new addresses are issued,
  and root creation can recover from a failed wallet import.
  Wallet-import errors are ignored only when they identify the exact script
  that lnd already watches.

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

- Boris Nagaev
- claravanstaden
- Slyghtning
