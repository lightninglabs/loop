# Loop Client Release Notes

- **Release date:** 2025-10-31
- **Release page:**
  [v0.31.5-beta](https://github.com/lightninglabs/loop/releases/tag/v0.31.5-beta)
- **Previous release:** [v0.31.4-beta](release-notes-0.31.4.md)
- **Next release:**
  [v0.31.5-beta-lnd0.20](release-notes-0.31.5-lnd0.20.md)

#### New Features

* Add notification versioning and a manager for resuming swaps.
  [PR #1029](https://github.com/lightninglabs/loop/pull/1029)
* Add Instant Out and static address RPCs to `client.yaml`.
  [PR #1026](https://github.com/lightninglabs/loop/pull/1026)

#### Breaking Changes

* Standardize static address amount flags on `--amt`.
  [PR #1023](https://github.com/lightninglabs/loop/pull/1023)

#### Bug Fixes

* Avoid an Instant Out startup crash when experimental features are disabled.
  [PR #1030](https://github.com/lightninglabs/loop/pull/1030)

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Boris Nagaev
