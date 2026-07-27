# Loop Client Release Notes

- **Release date:** 2020-05-12
- **Release page:**
  [v0.6.2-beta](https://github.com/lightninglabs/loop/releases/tag/v0.6.2-beta)
- **Previous release:** [v0.6.1-beta](release-notes-0.6.1.md)
- **Next release:** [v0.6.3-beta](release-notes-0.6.3.md)

#### New Features

* Switch Loop In to use native SegWit P2WSH HTLC addresses. Externally funded
  Loop Ins can use either nested SegWit or native SegWit addresses.
  [PR #184](https://github.com/lightninglabs/loop/pull/184)

#### Breaking Changes

#### Bug Fixes

* Fix the maximum number of payment parts for Multi-Loop Out so that the
  configured limit takes effect.
  [PR #196](https://github.com/lightninglabs/loop/pull/196)

#### Maintenance

* Apply follow-up version updates for v0.6.1-beta and v0.6.2-beta.
  [PR #199](https://github.com/lightninglabs/loop/pull/199)
  [PR #200](https://github.com/lightninglabs/loop/pull/200)

The user-facing changes highlighted in the published v0.6.2-beta release were
already present in the v0.6.1-beta tag history. The tag-to-tag source delta for
v0.6.2-beta contains the version metadata updates above.

#### Contributors (Alphabetical Order)

- Joost Jager
