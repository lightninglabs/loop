# Loop Client Release Notes

- **Release date:** 2022-03-30
- **Release page:**
  [v0.18.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.18.0-beta)
- **Previous release:** [v0.17.0-beta](release-notes-0.17.0.md)
- **Next release:** [v0.19.1-beta](release-notes-0.19.1.md)

#### New Features

* Loop client now supports optional routing plugins to improve off-chain payment
  reliability. One such plugin that the client implements will gradually prefer
  increasingly more expensive routes in case payments using cheap routes time
  out. Note that with this addition the minimum required LND version is LND
  0.14.2-beta.

#### Breaking Changes

#### Bug Fixes

* Loop now supports being hooked up to a remote signing pair of `lnd` nodes, as
  long as `lnd` is `v0.14.3-beta` or later.

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Harsha Goli
- Marnix
- Oliver Gugger
- Yong Yu
