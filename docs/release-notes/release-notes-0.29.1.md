# Loop Client Release Notes

- **Release date:** 2025-03-18
- **Release page:**
  [v0.29.1-beta](https://github.com/lightninglabs/loop/releases/tag/v0.29.1-beta)
- **Previous release:** [v0.29.0-beta](release-notes-0.29.0.md)
- **Next release:** [v0.30.0-beta](release-notes-0.30.0.md)

#### New Features

* Add Taproot Asset Loop Outs, including asset-aware quotes, accounting, and
  command-line options.
  [PR #872](https://github.com/lightninglabs/loop/pull/872)
  [PR #876](https://github.com/lightninglabs/loop/pull/876)
* Allow a specific amount to be withdrawn from static address deposits.
  [PR #860](https://github.com/lightninglabs/loop/pull/860)

#### Breaking Changes

#### Bug Fixes

* Fix Loop Out fee estimation when the confirmation target is one.
  [PR #899](https://github.com/lightninglabs/loop/pull/899)
* Add notification backoff and pending-L402 handling to avoid busy looping.
  [PR #879](https://github.com/lightninglabs/loop/pull/879)

#### Maintenance

* Update the compile-time LND dependency to v0.19.0.

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Boris Nagaev
- Oliver Gugger
- Slyghtning
- sputn1ck
