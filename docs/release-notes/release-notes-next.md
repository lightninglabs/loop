# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* Hardened Instant Out and reservation handling against malformed server
  responses, invalid signatures and reservation outputs, resource exhaustion,
  unsafe recovery, excessive swap fees, and under-scoped macaroon permissions.

* Taproot Asset Loop Out handling now validates RFQ timeouts and asset rates,
  keeps cached asset-name lookups responsive during slow `tapd` queries, and
  closes `tapd` connections cleanly during shutdown and startup failures.
  [PR #1189](https://github.com/lightninglabs/loop/pull/1189)

#### Maintenance

* Added a CI gate and repository agent guidance requiring every pull request to
  include a non-empty entry in the next release notes unless it carries the
  `no-changelog` label.

#### Contributors (Alphabetical Order)
