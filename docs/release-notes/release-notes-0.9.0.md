# Loop Client Release Notes

- **Release date:** 2020-09-10
- **Release page:**
  [v0.9.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.9.0-beta)
- **Previous release:** [v0.8.1-beta](release-notes-0.8.1.md)
- **Next release:** [v0.10.0-beta](release-notes-0.10.0.md)

#### New Features

- A liquidity management system is introduced to drive automated Loop actions
  based on desired liquidity
  - Fully automated Loop actions are not yet included, this release introduces
    suggested actions that can be executed using the API, or in the future can
    be directed to be executed automatically.
- A rewrite of the on-chain script is introduced that is a bit cheaper on-chain
  in the successful case

#### Breaking Changes

In this patch release:

- TLS encryption is introduced and is now required to communicate with the Loop
  daemon API
-  Support is discontinued in binary releases for Darwin 386 (32 bit Mac)

#### Bug Fixes

- The Docker image Go version is increased to Go 1.13

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Carla Kirk-Cohen
- Joost Jager
- Oliver Gugger
- Tom Dickman
