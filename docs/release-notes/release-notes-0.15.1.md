# Loop Client Release Notes

- **Release date:** 2021-11-18
- **Release page:**
  [v0.15.1-beta](https://github.com/lightninglabs/loop/releases/tag/v0.15.1-beta)
- **Previous release:** [v0.15.0-beta](release-notes-0.15.0.md)
- **Next release:** [v0.16.0-beta](release-notes-0.16.0.md)

#### New Features

* Add JSON client stubs for using the Loop RPC interface from WebAssembly.
  [PR #411](https://github.com/lightninglabs/loop/pull/411)
* Export `NewListenerConfig` so callers can construct custom listeners.
  [PR #432](https://github.com/lightninglabs/loop/pull/432)

#### Breaking Changes

#### Bug Fixes

* Improve errors for explicitly configured files.
  [PR #413](https://github.com/lightninglabs/loop/pull/413)
* Create the default macaroon only when required by the active configuration.
  [PR #428](https://github.com/lightninglabs/loop/pull/428)

#### Maintenance

* Refactor Autoloop swap construction behind a swap-builder interface.
  [PR #418](https://github.com/lightninglabs/loop/pull/418)
* Update `lndclient` and the compile-time LND dependency for LND v0.14.
  [PR #425](https://github.com/lightninglabs/loop/pull/425)
  [PR #426](https://github.com/lightninglabs/loop/pull/426)
  [PR #438](https://github.com/lightninglabs/loop/pull/438)

#### Contributors (Alphabetical Order)

- Carla Kirk-Cohen
- Harsha Goli
- Martin Habovštiak
- Oliver Gugger
- Turtle
- Yong Yu
