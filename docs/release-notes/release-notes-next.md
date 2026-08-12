# Loop Client Release Notes

#### New Features

#### Breaking Changes

* Instant Out and reservation RPCs now require the `loop:out` permission.
  Operators using custom scoped macaroons must rebake them before calling
  `ListReservations`, `InstantOut`, `InstantOutQuote`, or `ListInstantOuts`.
  [PR #1194](https://github.com/lightninglabs/loop/pull/1194)

#### Bug Fixes

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

* Added a CI gate and repository agent guidance requiring every pull request to
  include a non-empty entry in the next release notes unless it carries the
  `no-changelog` label.

#### Contributors (Alphabetical Order)
