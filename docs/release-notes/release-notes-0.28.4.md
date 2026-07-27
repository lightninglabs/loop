# Loop Client Release Notes

- **Release date:** 2024-06-04
- **Release page:**
  [v0.28.4-beta](https://github.com/lightninglabs/loop/releases/tag/v0.28.4-beta)
- **Previous release:** [v0.28.3-beta](release-notes-0.28.3.md)
- **Next release:** [v0.28.5-beta](release-notes-0.28.5.md)

#### New Features

* Make the LND RPC timeout configurable.
  [PR #771](https://github.com/lightninglabs/loop/pull/771)

#### Breaking Changes

#### Bug Fixes

* Correct the cost migration for pending swaps.
  [PR #771](https://github.com/lightninglabs/loop/pull/771)

#### Maintenance

* Factor out a sweep-batcher implementation that does not depend on the Loop
  database and update LND to v0.18.0-beta.1.
  [PR #766](https://github.com/lightninglabs/loop/pull/766)
  [PR #770](https://github.com/lightninglabs/loop/pull/770)

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Boris Nagaev
- sputn1ck
