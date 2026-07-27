# Loop Client Release Notes

- **Release date:** 2025-10-16
- **Release page:**
  [v0.31.4-beta](https://github.com/lightninglabs/loop/releases/tag/v0.31.4-beta)
- **Previous release:** [v0.31.3-beta](release-notes-0.31.3.md)
- **Next release:** [v0.31.5-beta](release-notes-0.31.5.md)

#### New Features

* Add fast publication support to traditional and static Loop Ins.
  [PR #1009](https://github.com/lightninglabs/loop/pull/1009)
* Add reproducible host and Docker release builds.
  [PR #1007](https://github.com/lightninglabs/loop/pull/1007)

#### Breaking Changes

* Migrate the command-line client to `urfave/cli/v3`.
  [PR #1013](https://github.com/lightninglabs/loop/pull/1013)

#### Bug Fixes

* Correct sweep-batcher change-output fee accounting and publish fee-rate
  calculation.
  [PR #1020](https://github.com/lightninglabs/loop/pull/1020)
* Harden static deposit recovery, withdrawal, and swap-state handling.

#### Maintenance

* Presign sweep transactions in parallel.
  [PR #1022](https://github.com/lightninglabs/loop/pull/1022)

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Boris Nagaev
- George Tsagkarelis
- Slyghtning
