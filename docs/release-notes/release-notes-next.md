# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* Static Address deposit reconciliation now preserves authoritative
  first-confirmation heights while lnd is catching up, preventing premature
  expiry decisions from mismatched wallet and block-notification heights.

* Loop In commands now parse `--route_hints` as a single JSON array and pass
  every route and hop through unchanged.

* `loopd --version` inside the official Docker images now reports the commit
  it was built from instead of an empty string.

* The official `linux/arm64` Docker images now contain arm64 binaries and an
  arm64 userspace. Every published platform was previously built for amd64, so
  `loopd` failed with `exec format error` on ARM hosts.
  [Issue #1211](https://github.com/lightninglabs/loop/issues/1211)

#### Maintenance

* The Docker image build now verifies that every platform of the image index
  holds binaries for the architecture it advertises, and gives a release its
  tag only once that check has passed.
  [Issue #1211](https://github.com/lightninglabs/loop/issues/1211)

#### Contributors (Alphabetical Order)
