# Loop Client Release Notes

- **Release date:** 2020-05-12
- **Release page:**
  [v0.6.1-beta](https://github.com/lightninglabs/loop/releases/tag/v0.6.1-beta)
- **Previous release:** [v0.6.0-beta](release-notes-0.6.0.md)
- **Next release:** [v0.6.2-beta](release-notes-0.6.2.md)

#### New Features

* Add native SegWit P2WSH addresses for Loop In HTLCs while retaining support
  for externally funded nested SegWit addresses.
  [PR #184](https://github.com/lightninglabs/loop/pull/184)

#### Breaking Changes

#### Bug Fixes

* Return the correct HTLC address in the Loop In swap response.
  [PR #198](https://github.com/lightninglabs/loop/pull/198)
* Enforce the configured maximum number of payment parts for multi-channel
  Loop Outs.
  [PR #196](https://github.com/lightninglabs/loop/pull/196)

#### Maintenance

* Configure CI to clone the full repository history.
  [PR #192](https://github.com/lightninglabs/loop/pull/192)

#### Contributors (Alphabetical Order)

- Andras Banki-Horvath
- Carla Kirk-Cohen
- Joost Jager
