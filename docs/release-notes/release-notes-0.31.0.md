# Loop Client Release Notes

- **Release date:** 2025-04-25
- **Release page:**
  [v0.31.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.31.0-beta)
- **Previous release:** [v0.30.0-beta](release-notes-0.30.0.md)
- **Next release:** [v0.31.1-beta](release-notes-0.31.1.md)

#### New Features

* [Enhance](https://github.com/lightninglabs/loop/pull/912) the `loop listswaps`
  command by improving the ability to filter the response. Use
  `--start_timestamp_ns` to return only swaps after that timestamp. Use
  `--max_swaps` to limit total swap outputs. Paging is enabled using the
  `next_start_time` field in the response.

#### Breaking Changes

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Boris Nagaev
- Guillermo Caracuel
- Sam Korn
- Slyghtning
