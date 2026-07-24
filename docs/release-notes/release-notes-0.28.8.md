# Loop Client Release Notes

- **Release date:** 2024-10-21
- **Release page:**
  [v0.28.8-beta](https://github.com/lightninglabs/loop/releases/tag/v0.28.8-beta)
- **Previous release:** [v0.28.7-beta](release-notes-0.28.7.md)
- **Next release:** [v0.28.9-beta](release-notes-0.28.9.md)

#### New Features

* Add a persistent notification manager and generalize the server notification
  stream.
  [PR #826](https://github.com/lightninglabs/loop/pull/826)
* Add mixed sweep batches, configurable publication delays, transaction labels,
  and a maximum batch size.
  [PR #791](https://github.com/lightninglabs/loop/pull/791)
  [PR #801](https://github.com/lightninglabs/loop/pull/801)
  [PR #809](https://github.com/lightninglabs/loop/pull/809)
  [PR #813](https://github.com/lightninglabs/loop/pull/813)

#### Breaking Changes

#### Bug Fixes

* Prevent sweep-batcher fee rates from decreasing and improve publication error
  handling.
  [PR #815](https://github.com/lightninglabs/loop/pull/815)
  [PR #833](https://github.com/lightninglabs/loop/pull/833)

#### Maintenance

* Publish `looprpc` as a separately versioned Go module.

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Boris Nagaev
- drawdrop
- Slyghtning
- sputn1ck
