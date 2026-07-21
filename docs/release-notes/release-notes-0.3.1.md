# Loop Client Release Notes

- **Release date:** 2020-01-15
- **Release page:**
  [v0.3.1-alpha](https://github.com/lightninglabs/loop/releases/tag/v0.3.1-alpha)
- **Previous release:** [v0.3.0-alpha](release-notes-0.3.0.md)
- **Next release:** [v0.4.0-rc1.beta](release-notes-0.4.0-rc1.md)

#### New Features

In this patch version release:

- `loop out` now supports an optional `--max_swap_routing_fee` flag to specify a
  max routing fee
- `loop quote` now supports an optional `--fast` flag to specify a quote for an
  immediate swap

This version is recommended for all users to improve the fidelity of fee
estimation. In a previous update the swap quote may have returned a higher
estimated fee than would have been ultimately paid.

Max swap routing fee is also recommended for users who want to adjust the fee
ceiling of their Loop Out Lightning Network fees.

#### Breaking Changes

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- FreeThinker
- Johan T. Halseth
- Oliver Gugger
- Wilmer Paulino
