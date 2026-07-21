# Loop Client Release Notes

- **Release date:** 2024-02-14
- **Release page:**
  [v0.27.1-beta](https://github.com/lightninglabs/loop/releases/tag/v0.27.1-beta)
- **Previous release:** [v0.27.0-beta](release-notes-0.27.0.md)
- **Next release:** [v0.28.0-beta](release-notes-0.28.0.md)

#### New Features

#### Breaking Changes

#### Bug Fixes

This release adds automatic sweeping of incorrectly deposited amounts to
external loop in addresses. Previously, a mismatch in the contract amount and
the actually deposited amount required external tools to recover the client
funds. With this release the client automatically sweeps the funds back to the
wallet upon contract expiry.

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Slyghtning
- sputn1ck
