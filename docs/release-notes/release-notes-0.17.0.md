# Loop Client Release Notes

- **Release date:** 2022-02-07
- **Release page:**
  [v0.17.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.17.0-beta)
- **Previous release:** [v0.16.0-beta](release-notes-0.16.0.md)
- **Next release:** [v0.18.0-beta](release-notes-0.18.0.md)

#### New Features

* Loop in functionality has been added to AutoLoop. This feature can be enabled
  to acquire outgoing capacity on your node automatically, using
  `loop setrule --type=in`. At present, autoloop can only be set to loop out
  *or* loop in, and cannot manage liquidity in both directions.

* Use LND's hop hint selector when doing private loop-ins.

#### Breaking Changes

#### Bug Fixes

* Close local databases when loopd daemon is stopped programmatically.

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Carla Kirk-Cohen
- Elle Mouton
- Suhail Saqan
