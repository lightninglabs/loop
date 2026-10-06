# Loop Client Release Notes

#### New Features

* Instant Out now validates server invoices against a caller-approved maximum
  swap fee.

* Loop In can now receive over a Taproot Asset channel. Asset swaps require an
  explicit edge and minimum asset output, reuse the swap RFQ for the probe,
  and require a quote valid for the 30-day invoice lifetime before funding.

#### Breaking Changes

* Instant Out requests must now set `max_swap_fee_sat`. Requests that omit the
  fee cap are rejected; an explicit zero cap remains valid. Direct users of
  `Manager.NewInstantOut` must pass the fee cap as a required argument.

* Raise the minimum supported LND version to v0.19.0-beta. Since v0.33.3,
  `loopd` could not buy its L402 token on older LND, which rejected the
  payment with `timeout_seconds must be specified`, so a `loopd` without a
  paid token could not use the Loop server.

#### Bug Fixes

* Sweep batch selection now filters out batches with an incompatible signing
  mode before attempting admission, avoiding spurious warnings when regular
  and presigned sweeps are pending together.

* A presigned sweep batch that can't admit a sweep because its co-signers did
  not sign in time now logs the rejection at info level instead of as a
  warning. Such sweeps are offered to another batch or a new one. Rejections
  during shutdown are logged at info level too; other presigning failures
  remain warnings.

* Instant Out now attempts to cancel server-side swaps when client
  initialization fails, allowing locked reservations to be released without
  waiting for the server timeout.

* Static-address loop-in quotes and manual outpoint initiation now reject
  deposits that are too close to expiry before contacting the Loop server.

* Static Address deposit reconciliation now preserves authoritative
  first-confirmation heights while lnd is catching up, preventing premature
  expiry decisions from mismatched wallet and block-notification heights.

* Rapid reservation funding confirmations no longer cause initialization to
  time out while waiting for an intermediate client state.

* Loop In commands now parse `--route_hints` as a single JSON array and pass
  every route and hop through unchanged.

* `loopd --version` inside the official Docker images now reports the commit
  it was built from instead of an empty string.

* The official `linux/arm64` Docker images now contain arm64 binaries and an
  arm64 userspace. Every published platform was previously built for amd64, so
  `loopd` failed with `exec format error` on ARM hosts.
  [Issue #1211](https://github.com/lightninglabs/loop/issues/1211)

* When a Loop Out prepayment cannot be routed, the client now reports the
  failure to the server as a prepay routing failure. It was previously
  reported as a failure to route the swap invoice.

* A fee bump of a static address withdrawal without an address now pays
  the address of the withdrawal it replaces. It paid a new wallet address
  before, and the client never recognized its confirmation, so the deposits
  stayed in the withdrawing state. A fee bump to a different address is
  rejected.

#### Maintenance

* Align the standalone `looprpc` module's OpenTelemetry SDK and OTLP trace
  exporters at v1.45.0, matching the versions used by the root module.
  Refresh the generated REST bindings and OpenAPI specification for the
  required grpc-gateway upgrade.
  [PR #1235](https://github.com/lightninglabs/loop/pull/1235)

* Update the `looprpc` gRPC dependency to v1.83.2 and synchronize the
  root module with its required dependencies.
  [PR #1227](https://github.com/lightninglabs/loop/pull/1227)

* Update the `swapserverrpc` gRPC dependency to v1.83.2 and synchronize
  the root module with its required dependencies.
  [PR #1226](https://github.com/lightninglabs/loop/pull/1226)

* Update the OpenTelemetry OTLP gRPC trace exporter from v1.20.0 to
  v1.45.0 and refresh its required dependencies.
  [PR #1230](https://github.com/lightninglabs/loop/pull/1230)

* Update the OpenTelemetry OTLP trace exporter from v1.29.0 to v1.45.0
  and refresh its required dependencies.
  [PR #1231](https://github.com/lightninglabs/loop/pull/1231)

* Update the OpenTelemetry SDK and its related API modules to v1.45.0.
  [PR #1232](https://github.com/lightninglabs/loop/pull/1232)

* The Docker image build now verifies that every platform of the image index
  holds binaries for the architecture it advertises, and gives a release its
  tag only once that check has passed.
  [Issue #1211](https://github.com/lightninglabs/loop/issues/1211)

* The regtest environment now runs LND v0.21.0-beta, the version Loop is built
  against, instead of v0.18.5-beta.

#### Contributors (Alphabetical Order)
