# Loop Client Release Notes

- **Release date:** 2026-02-03
- **Release page:**
  [v0.31.8-beta](https://github.com/lightninglabs/loop/releases/tag/v0.31.8-beta)
- **Previous release:** [v0.31.7-beta](release-notes-0.31.7.md)
- **Next release:** [v0.32.0-beta](release-notes-0.32.0.md)

#### New Features

* Add the `SweepHtlc` RPC and `loop out sweephtlc` command for manually
  sweeping a Loop Out HTLC.
  [PR #1066](https://github.com/lightninglabs/loop/pull/1066)
* Add PSBT-based static address withdrawals.
  [PR #1043](https://github.com/lightninglabs/loop/pull/1043)

#### Breaking Changes

* Deprecate the previous server-driven static withdrawal RPC in favor of the
  PSBT-based endpoint.
  [PR #1043](https://github.com/lightninglabs/loop/pull/1043)

#### Bug Fixes

* Re-register static address HTLC notifications after confirmation errors.
* Persist confirmed sweep batches atomically.
  [PR #1044](https://github.com/lightninglabs/loop/pull/1044)

#### Maintenance

* Replace deprecated gRPC dial helpers and remove unused client code.

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Boris Nagaev
- Slyghtning
- Viktor Torstensson
