# Loop Client Release Notes

- **Release date:** 2026-05-26
- **Release page:**
  [v0.33.1-beta](https://github.com/lightninglabs/loop/releases/tag/v0.33.1-beta)
- **Previous release:** [v0.33.0-beta](release-notes-0.33.0.md)
- **Next release:** [v0.33.2-beta](release-notes-0.33.2.md)

#### New Features

* Add Static Address Loop Ins to Autoloop planning and execution.
  [PR #1119](https://github.com/lightninglabs/loop/pull/1119)

#### Breaking Changes

#### Bug Fixes

* Validate static address server parameters and prevent panics when displaying
  malformed or missing Instant Out reservation identifiers.
  [PR #1137](https://github.com/lightninglabs/loop/pull/1137)
* Harden sweep-batcher shutdown and route-hint preservation.

#### Maintenance

* Add record-and-replay integration tests for CLI sessions.
  [PR #1072](https://github.com/lightninglabs/loop/pull/1072)
* Add the project security policy.

#### Contributors (Alphabetical Order)

- 0xfandom
- Alex Bosworth
- Boris Nagaev
- Olexandr88
- Slyghtning
