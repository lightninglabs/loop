# Loop Client Release Notes

- **Release date:** 2025-09-25
- **Release page:**
  [v0.31.3-beta](https://github.com/lightninglabs/loop/releases/tag/v0.31.3-beta)
- **Previous release:** [v0.31.2-beta](release-notes-0.31.2.md)
- **Next release:** [v0.31.4-beta](release-notes-0.31.4.md)

#### New Features

* Allow static address Loop Ins to swap an amount smaller than the selected
  deposits and expose arbitrary static swap amounts through the server RPC.
  [PR #887](https://github.com/lightninglabs/loop/pull/887)
  [PR #951](https://github.com/lightninglabs/loop/pull/951)
* Add deposit indices, outpoint-based deposit lookups, and CLI output for
  deposits and their swap hashes.
  [PR #959](https://github.com/lightninglabs/loop/pull/959)
  [PR #990](https://github.com/lightninglabs/loop/pull/990)
  [PR #991](https://github.com/lightninglabs/loop/pull/991)
* Add sweep-batcher change outputs.
  [PR #976](https://github.com/lightninglabs/loop/pull/976)

#### Breaking Changes

#### Bug Fixes

* Harden static address deposit recovery, withdrawal handling, block polling,
  expiry tracking, and deposit availability filtering.
  [PR #954](https://github.com/lightninglabs/loop/pull/954)
  [PR #956](https://github.com/lightninglabs/loop/pull/956)
  [PR #995](https://github.com/lightninglabs/loop/pull/995)
  [PR #996](https://github.com/lightninglabs/loop/pull/996)
  [PR #1003](https://github.com/lightninglabs/loop/pull/1003)
  [PR #1004](https://github.com/lightninglabs/loop/pull/1004)
* Avoid creating a Loop In timeout transaction after its invoice has already
  been paid.
  [PR #998](https://github.com/lightninglabs/loop/pull/998)
* Fix sweep-batcher reorg detection and presigned-sweep handling.
  [PR #952](https://github.com/lightninglabs/loop/pull/952)
  [PR #975](https://github.com/lightninglabs/loop/pull/975)
* Avoid a startup panic when the Taproot Asset client is unavailable.
  [PR #970](https://github.com/lightninglabs/loop/pull/970)

#### Maintenance

* Update LND through v0.19.3, Taproot Assets through v0.6.1, Go, and lint
  tooling.
  [PR #985](https://github.com/lightninglabs/loop/pull/985)
  [PR #989](https://github.com/lightninglabs/loop/pull/989)
  [PR #993](https://github.com/lightninglabs/loop/pull/993)
  [PR #1001](https://github.com/lightninglabs/loop/pull/1001)
* Build static release binaries and fix release-version matching.
  [PR #965](https://github.com/lightninglabs/loop/pull/965)
  [PR #978](https://github.com/lightninglabs/loop/pull/978)

#### Contributors (Alphabetical Order)

- Andras Banki-Horvath
- Boris Nagaev
- George Tsagkarelis
- Slyghtning
