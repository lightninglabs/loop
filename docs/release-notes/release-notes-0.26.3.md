# Loop Client Release Notes

- **Release date:** 2023-09-18
- **Release page:**
  [v0.26.3-beta](https://github.com/lightninglabs/loop/releases/tag/v0.26.3-beta)
- **Previous release:** [v0.26.2-beta](release-notes-0.26.2.md)
- **Next release:** [v0.26.4-beta](release-notes-0.26.4.md)

#### New Features

* Add the reusable finite state machine module.
  [PR #631](https://github.com/lightninglabs/loop/pull/631)

#### Breaking Changes

#### Bug Fixes

* Treat an already-settled Loop In invoice as a successful settlement.
  [PR #636](https://github.com/lightninglabs/loop/pull/636)
* Correct SQL time parsing and the faulty-year migration, and skip bbolt
  migration when the SQLite database already exists.
  [PR #627](https://github.com/lightninglabs/loop/pull/627)
  [PR #630](https://github.com/lightninglabs/loop/pull/630)

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Liongrass
- Slyghtning
- sputn1ck
