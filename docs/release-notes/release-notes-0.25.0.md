# Loop Client Release Notes

- **Release date:** 2023-07-03
- **Release page:**
  [v0.25.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.25.0-beta)
- **Previous release:** [v0.24.1-beta](release-notes-0.24.1.md)
- **Next release:** [v0.25.1-beta](release-notes-0.25.1.md)

#### New Features

* Add SQLite and PostgreSQL stores, including migration from an existing bbolt
  database.
  [PR #585](https://github.com/lightninglabs/loop/pull/585)
* Expose the L402 token identifier through the gRPC interface.
  [PR #601](https://github.com/lightninglabs/loop/pull/601)
* Distinguish swaps initiated by Easy Autoloop in their labels.
  [PR #591](https://github.com/lightninglabs/loop/pull/591)

#### Breaking Changes

#### Bug Fixes

* Respect the configured fee-PPM limit in Easy Autoloop.
  [PR #595](https://github.com/lightninglabs/loop/pull/595)
* Read the Autoloop enabled flag directly from the active liquidity
  parameters.
  [PR #590](https://github.com/lightninglabs/loop/pull/590)

#### Maintenance

* Correct Autoloop documentation and harden its tests.
  [PR #593](https://github.com/lightninglabs/loop/pull/593)
  [PR #594](https://github.com/lightninglabs/loop/pull/594)
  [PR #596](https://github.com/lightninglabs/loop/pull/596)

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- George Tsagkarelis
- Slyghtning
- sputn1ck
