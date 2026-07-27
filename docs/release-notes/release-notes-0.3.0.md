# Loop Client Release Notes

- **Release date:** 2019-11-21
- **Release page:**
  [v0.3.0-alpha](https://github.com/lightninglabs/loop/releases/tag/v0.3.0-alpha)
- **Previous release:** [v0.2.4-alpha](release-notes-0.2.4.md)
- **Next release:** [v0.3.1-alpha](release-notes-0.3.1.md)

#### New Features

This minor version release includes:

- Change the default behavior of Loop Out swaps to use the new Loop Out delay
- New `--fast` flag to execute Loop Out swaps immediately
- Add delay configuration to the Loop Out client API

The new delay option is related to an on-going effort to minimize the on-chain
footprint of Lightning Loop, which will deliver increased privacy and lower
chain fees.

If multiple execution delayed Loop Outs are present at the time of Loop Out
on-chain funding, the Loop service will now batch those funding outputs together
into a single transaction, reducing the number of change outputs and required
inputs used for each swap.

#### Breaking Changes

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Corey Phillips
- Johan T. Halseth
- Joost Jager
- Oliver Gugger
- Wilmer Paulino
