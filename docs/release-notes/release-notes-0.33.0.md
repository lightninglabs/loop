# Loop Client Release Notes

- **Release date:** 2026-04-08
- **Release page:**
  [v0.33.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.33.0-beta)
- **Previous release:** [v0.32.1-beta](release-notes-0.32.1.md)
- **Next release:** [v0.33.1-beta](release-notes-0.33.1.md)

#### New Features

* Add maximum swap-fee controls to static address Loop Ins.
  [PR #1114](https://github.com/lightninglabs/loop/pull/1114)

#### Breaking Changes

#### Bug Fixes

* Harden static address startup, withdrawal notification registration, Loop In
  recovery, HTLC timeout handling, and sweep output validation.
  [PR #1078](https://github.com/lightninglabs/loop/pull/1078)
* Ensure cleanup operations use live contexts and fix a Loop Out test goroutine
  leak.

#### Maintenance

* Regenerate the command-line reference and update build dependencies.

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Boris Nagaev
- Slyghtning
