# Loop Client Release Notes

- **Release date:** 2025-11-26
- **Release page:**
  [v0.31.6-beta](https://github.com/lightninglabs/loop/releases/tag/v0.31.6-beta)
- **Previous release:**
  [v0.31.5-beta-lnd0.20](release-notes-0.31.5-lnd0.20.md)
- **Next release:** [v0.31.7-beta](release-notes-0.31.7.md)

#### New Features

* Add Easy Autoloop peer exclusion and an option to include all peers.
  [PR #1016](https://github.com/lightninglabs/loop/pull/1016)
* Add the `loop stop` command.
  [PR #1048](https://github.com/lightninglabs/loop/pull/1048)

#### Breaking Changes

#### Bug Fixes

* Wait for the chain notifier and a positive block height during startup.
  [PR #1046](https://github.com/lightninglabs/loop/pull/1046)
* Correct a sweep-batcher race while re-adding sweeps during shutdown.
  [PR #1042](https://github.com/lightninglabs/loop/pull/1042)

#### Maintenance

* Update LND to v0.20.0-beta and Taproot Assets to v0.7.0.
  [PR #1045](https://github.com/lightninglabs/loop/pull/1045)
* Update `lndclient`, Go, and security-sensitive dependencies.
  [PR #1034](https://github.com/lightninglabs/loop/pull/1034)
  [PR #1041](https://github.com/lightninglabs/loop/pull/1041)
  [PR #1047](https://github.com/lightninglabs/loop/pull/1047)
  [PR #1049](https://github.com/lightninglabs/loop/pull/1049)
* Add Lightning Terminal integration tests to CI.
  [PR #1050](https://github.com/lightninglabs/loop/pull/1050)

#### Contributors (Alphabetical Order)

- Boris Nagaev
- ffranr
- Slyghtning
