# Loop Client Release Notes

- **Release date:** 2026-06-08
- **Release page:**
  [v0.33.2-beta](https://github.com/lightninglabs/loop/releases/tag/v0.33.2-beta)
- **Previous release:** [v0.33.1-beta](release-notes-0.33.1.md)
- **Next release:** [v0.33.3-beta](release-notes-0.33.3.md)

#### New Features

* Expose static address swap timing and cost details through the RPC and CLI.
  [PR #1150](https://github.com/lightninglabs/loop/pull/1150)
* Handle server notifications that a static address HTLC has confirmed.
  [PR #1120](https://github.com/lightninglabs/loop/pull/1120)

#### Breaking Changes

#### Bug Fixes

* Reject malformed server keys and MuSig2 signing data.
  [PR #1148](https://github.com/lightninglabs/loop/pull/1148)
* Recover confirmed HTLCs through the direct sweep path and reuse their stored
  destinations.

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Boris Nagaev
- Slyghtning
