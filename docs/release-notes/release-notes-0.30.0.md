# Loop Client Release Notes

- **Release date:** 2025-03-27
- **Release page:**
  [v0.30.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.30.0-beta)
- **Previous release:** [v0.29.1-beta](release-notes-0.29.1.md)
- **Next release:** [v0.31.0-beta](release-notes-0.31.0.md)

#### New Features

* Add Taproot Asset support to Easy Autoloop Loop Outs.
  [PR #886](https://github.com/lightninglabs/loop/pull/886)

#### Breaking Changes

#### Bug Fixes

* Make static address deposit recovery non-concurrent and correct deposit FSM
  shutdown and locking behavior.
  [PR #908](https://github.com/lightninglabs/loop/pull/908)
* Fix data races across Loop Out payment, reservations, static addresses,
  logging, and sweep batching.
  [PR #890](https://github.com/lightninglabs/loop/pull/890)
  [PR #902](https://github.com/lightninglabs/loop/pull/902)

#### Maintenance

* Run unit tests with the Go race detector.

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Boris Nagaev
- Slyghtning
- sputn1ck
