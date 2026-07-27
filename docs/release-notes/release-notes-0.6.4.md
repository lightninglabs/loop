# Loop Client Release Notes

- **Release date:** 2020-06-12
- **Release page:**
  [v0.6.4-beta](https://github.com/lightninglabs/loop/releases/tag/v0.6.4-beta)
- **Previous release:** [v0.6.3-beta](release-notes-0.6.3.md)
- **Next release:** [v0.6.5-beta](release-notes-0.6.5.md)

#### New Features

#### Breaking Changes

#### Bug Fixes

In this patch release:

- A loopd error will now terminate with exit code 1 instead of exit code 0 to
  assist auto-restarting
- Loop Out will now increase inbound liquidity more quickly in situations where
  low chain fees were specified

If you have time to wait for the funds to return to you on-chain, consider
increasing `--conf_target` from the default of 6 when running the `loop out`
command to achieve fee savings at the cost of slower swap finality.

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Carla Kirk-Cohen
- Oliver Gugger
