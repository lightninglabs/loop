# Loop Client Release Notes

- **Release date:** 2024-06-03
- **Release page:**
  [v0.28.3-beta](https://github.com/lightninglabs/loop/releases/tag/v0.28.3-beta)
- **Previous release:** [v0.28.2-beta](release-notes-0.28.2.md)
- **Next release:** [v0.28.4-beta](release-notes-0.28.4.md)

#### New Features

#### Breaking Changes

#### Bug Fixes

* Migrate incorrectly stored negative Loop Out costs and account for the
  prepayment amount correctly.
  [PR #764](https://github.com/lightninglabs/loop/pull/764)
* Restore sweep-batcher swaps from the Loop database and preserve their
  confirmation targets.
  [PR #761](https://github.com/lightninglabs/loop/pull/761)

#### Maintenance

* Update the compile-time LND dependency to v0.18.0-beta.

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Boris Nagaev
- Slyghtning
