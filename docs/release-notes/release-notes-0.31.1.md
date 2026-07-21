# Loop Client Release Notes

- **Release date:** 2025-05-05
- **Release page:**
  [v0.31.1-beta](https://github.com/lightninglabs/loop/releases/tag/v0.31.1-beta)
- **Previous release:** [v0.31.0-beta](release-notes-0.31.0.md)
- **Next release:** [v0.31.2-beta](release-notes-0.31.2.md)

#### New Features

#### Breaking Changes

* Autoloop now defaults to a slow publication deadline (30 minutes after
  initiation) to reduce fees. To revert to the previous behavior, users can run
  `loop setparams --fast`, which causes swap HTLCs to be published immediately
  after initiation.

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Boris Nagaev
- ffranr
- Oliver Gugger
- Slyghtning
- sputn1ck
