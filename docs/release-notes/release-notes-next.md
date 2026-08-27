# Loop Client Release Notes

#### New Features

* The `loop out sweephtlc` recovery command can reconstruct and sweep a
  protocol-11 Loop Out HTLC from public swap data when the local swap database
  record is unavailable. It can also request the server's cooperative MuSig2
  signature to spend an original Taproot HTLC through its key path.

* Static Address now derives fresh receive and change addresses while retaining
  per-deposit address ownership across restarts for discovery, recovery, and
  signing. The new `loop static deposit` command can create and fund an address
  directly from the lnd wallet, and deposit listings identify the receiving
  address. [PR #1139](https://github.com/lightninglabs/loop/pull/1139)

#### Breaking Changes

* Instant Out and reservation RPCs now require the `loop:out` permission.
  Operators using custom scoped macaroons must rebake them before calling
  `ListReservations`, `InstantOut`, `InstantOutQuote`, or `ListInstantOuts`.
  [PR #1194](https://github.com/lightninglabs/loop/pull/1194)

* Calling `NewStaticAddress` without `send_coins_request.addr` now derives and
  returns a fresh receive address instead of reusing the address associated
  with the client's L402. Integrations must not assume that repeated calls are
  idempotent or return the same address. The RPC now requires the
  `swap:execute` permission instead of `swap:read`, including for address-only
  calls. Operators using custom scoped macaroons must rebake them accordingly.
  The deprecated `StaticAddressSummaryResponse.static_address` field remains
  the legacy/root address for compatibility and must not be treated as the
  current receive address; call `NewStaticAddress` to derive a fresh one.
  [PR #1139](https://github.com/lightninglabs/loop/pull/1139)

#### Bug Fixes

* Static Address withdrawals now follow the transaction that actually replaces
  an original withdrawal, reconcile partial conflicting spends, and wait for
  their background monitors during shutdown.

* Static Address startup now avoids reimporting wallet scripts that lnd already
  watches, address lookups remain responsive while new addresses are issued,
  and seed creation can recover from a failed wallet import.

* `loop static deposit` now resolves requested existing funding addresses
  directly by script, and wallet-import errors are ignored only when they
  identify the exact script that lnd already watches.

* Loop Out requests now account for channel reserves when checking outbound
  capacity, preventing swaps from starting when their off-chain payment cannot
  be funded.

* Improved Instant Out and reservation validation, lifecycle cleanup, recovery
  timing, fee limits, and macaroon permissions.
  [PR #1194](https://github.com/lightninglabs/loop/pull/1194)

* Taproot Asset Loop Out handling now validates RFQ timeouts and asset rates,
  keeps cached asset-name lookups responsive during slow `tapd` queries, and
  closes `tapd` connections cleanly during shutdown and startup failures.
  [PR #1189](https://github.com/lightninglabs/loop/pull/1189)

#### Maintenance

* Updated the gRPC dependency to v1.83.1.

* Updated the Taproot Assets dependency to v0.8.1; asset conversions that
  overflow a millisatoshi amount are now safely rejected.

* Added a CI gate and repository agent guidance requiring every pull request to
  include a non-empty entry in the next release notes unless it carries the
  `no-changelog` label.

#### Contributors (Alphabetical Order)
