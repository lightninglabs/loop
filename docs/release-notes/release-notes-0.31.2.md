# Loop Client Release Notes

- **Release date:** 2025-06-18
- **Release page:**
  [v0.31.2-beta](https://github.com/lightninglabs/loop/releases/tag/v0.31.2-beta)
- **Previous release:** [v0.31.1-beta](release-notes-0.31.1.md)
- **Next release:** [v0.31.3-beta](release-notes-0.31.3.md)

#### New Features

#### Breaking Changes

* The content of the `commit_hash` field of the `GetInfo` response has been
  updated so that it contains the Git commit hash the Loop binary build was
  based on. If the build had uncommited changes, this field will contain the
  most recent commit hash, suffixed by "-dirty".
* The `Commit` part of the `--version` command output has been updated to
  contain the most recent git commit tag.

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Boris Nagaev
- Oliver Gugger
- Slyghtning
- Viktor Tigerström
