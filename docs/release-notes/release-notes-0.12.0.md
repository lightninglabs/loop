# Loop Client Release Notes

- **Release date:** 2021-03-06
- **Release page:**
  [v0.12.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.12.0-beta)
- **Previous release:** [v0.11.4-beta](release-notes-0.11.4.md)
- **Next release:** [v0.12.1-beta](release-notes-0.12.1.md)

#### New Features

* Autoloop can now be configured on a per-peer basis, rather than only on an
  individual channel level. This change allows desired liquidity thresholds to
  be set for an individual peer, rather than a specific channel, and leverages
  multi-loop-out to more efficiently manage liquidity. To configure peer-level
  rules, provide the 'setrule' command with the peer's pubkey.
* Autoloop's fee API has been simplified to allow setting a single percentage
  which will be used to limit total swap fees to a percentage of the amount
  being swapped, the default budget has been updated to reflect this. Use
  `loop setparams --feepercent={percentage}` to update this value. This fee
  setting has been updated to the default for autoloop.
* The default confirmation target for automated loop out swap sweeps has been
  increased to 100 blocks. This change will not affect the time it takes to
  acquire inbound liquidity, but will decrease the cost of swaps.

#### Breaking Changes

#### Bug Fixes

* The loop dockerfile has been updated to use the `make` command so that the
  latest commit hash of the code being run will be included in `loopd`.
* A bug where loop in on-chain fees were not recorded properly has been
  addressed.

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Carla Kirk-Cohen
- rockstardev
