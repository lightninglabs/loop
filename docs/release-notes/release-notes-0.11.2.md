# Loop Client Release Notes

- **Release date:** 2020-12-08
- **Release page:**
  [v0.11.2-beta](https://github.com/lightninglabs/loop/releases/tag/v0.11.2-beta)
- **Previous release:** [v0.11.1-beta](release-notes-0.11.1.md)
- **Next release:** [v0.11.3-beta](release-notes-0.11.3.md)

#### New Features

**Autoloop Swap Size**

* Autoloop can now be configured with custom swap size limits. Previously,
  autoloop would use the minimum/maximum swap amount set by the server (exposed
  by the `loop terms` command) to decide on swap size.
* Setting a custom minimum swap amount is particularly useful for clients that
  would like to perform fewer, larger swaps to save on fees. The trade-off when
  setting a large minimum amount is that autoloop will wait until your channel
  is at least the minimum amount below its incoming threshold amount before
  executing a swap, which may result in channels staying under the threshold for
  longer.
* These values can be set using the following command:
  `loop setparams --minamt={minimum in sats} --maxamt={maximum in sats}`.
* The values set must fall within the limits set by the loop server.

#### Breaking Changes

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Carla Kirk-Cohen
