# Loop Client Release Notes

#### New Features

* Instant Out now validates server invoices against a caller-approved maximum
  swap fee.

* Static Address now derives fresh receive and change addresses while retaining
  per-deposit address ownership across restarts for discovery, recovery, and
  signing. The new `loop static deposit` command can create and fund an address
  directly from the lnd wallet, and deposit listings identify the receiving
  address. [PR #1218](https://github.com/lightninglabs/loop/pull/1218)

#### Breaking Changes

* Static-address wallet funding uses the new `FundStaticAddress` RPC, requiring
  `wallet:fund` in addition to `swap:execute` and `loop:in`, or an explicit URI
  grant for the new RPC. `NewStaticAddress` only creates addresses. Existing
  execute/in and address-creation URI macaroons do not gain wallet-spending
  authority. `loop static deposit` uses the new endpoint. The default local
  Loop macaroon is regenerated on startup when permissions change; operators
  must explicitly update distributed or custom macaroons intended for funding.
  [PR #1218](https://github.com/lightninglabs/loop/pull/1218)

* Instant Out requests must now set `max_swap_fee_sat`. Requests that omit the
  fee cap are rejected; an explicit zero cap remains valid. Direct users of
  `Manager.NewInstantOut` must pass the fee cap as a required argument.

* Calling `NewStaticAddress` now derives and returns a fresh receive address
  instead of reusing the address associated with the client's L402.
  Integrations must not assume repeated calls are idempotent or return the
  same address. The RPC now requires the
  `swap:execute` permission instead of `swap:read`, including for address-only
  calls. Operators using custom scoped macaroons must rebake them accordingly.
  The deprecated `StaticAddressSummaryResponse.static_address` field remains
  the legacy/root address for compatibility and must not be treated as the
  current receive address; call `NewStaticAddress` to derive a fresh one.
  [PR #1218](https://github.com/lightninglabs/loop/pull/1218)

#### Bug Fixes

* Sweep batch selection now filters out batches with an incompatible signing
  mode before attempting admission, avoiding spurious warnings when regular
  and presigned sweeps are pending together.

* The multi-address migration now rejects databases with existing deposits unless
  exactly one legacy static address is available to establish their ownership.

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

* Static Address startup now avoids reimporting wallet scripts that lnd already
  watches, address lookups remain responsive while new addresses are issued,
  and root creation can recover from a failed wallet import.

* The `FundStaticAddress` RPC can fund a requested existing static address by
  resolving it directly through the active script index. Wallet-import errors
  are ignored only when they identify the exact script that lnd already
  watches.

* Static Address loop-in sweep requests are now rejected unless the server
  provides exactly one prevout for every sweep input. A duplicate or missing
  prevout previously crashed `loopd` while computing the sweep signature
  hashes.

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

* Rename static-address Go APIs to `StaticSingleAddressKeyFamily`,
  `AddressParameters`, and `EnsureStaticAddressRoot`, and consolidate wallet
  UTXO listing into `ListUnspent`. Key-family values remain unchanged.
  [PR #1218](https://github.com/lightninglabs/loop/pull/1218)

* The Docker image build now verifies that every platform of the image index
  holds binaries for the architecture it advertises, and gives a release its
  tag only once that check has passed.
  [Issue #1211](https://github.com/lightninglabs/loop/issues/1211)

#### Contributors (Alphabetical Order)
