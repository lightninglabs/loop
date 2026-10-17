# Loop Client Release Notes

#### New Features

* The `--postgres.password` flag now accepts a `@/path/to/file` syntax that
  reads the password from a file instead of taking it on the command line, so
  the secret is no longer exposed in process listings or shell history. A
  password that begins with a literal `@` is written as `@@password`.
  [Issue #1088](https://github.com/lightninglabs/loop/issues/1088)

* Instant Out now validates server invoices against a caller-approved maximum
  swap fee.

* Static Address deposits and loop-ins now report their on-chain cost. A
  deposit's cost is its share of the fee of the transaction that funded it: the
  fee is split across the transaction's deposit outputs by value, so it is
  counted once even if the deposits are used in different swaps, and if the
  transaction also paid someone else, its whole fee is charged to the deposits.
  Change that a partial loop-in or withdrawal sends back to the static address
  costs nothing. If the client wallet doesn't know the funding fee, for example
  for a deposit funded by an external wallet, the deposit's cost is unknown. The
  new `cost_onchain` and `cost_onchain_status` fields of
  `ListStaticAddressDeposits` show a deposit's cost, also before it confirms. To
  add up the cost of deposits, use these deposits, since the deposits of swaps
  and withdrawals can include the same deposit. A static loop-in's
  `cost_onchain`, which was previously always zero, is the sum of the costs of
  the deposits it spent, including a deposit's whole fee share when only part of
  it is looped in. A swap that wasn't paid but whose htlc spent its deposits has
  the cost once its htlc timed out, also if it failed after publishing its htlc
  timeout sweep, but fees of the htlc timeout path are not included. Any other
  failed swap reports an unknown cost. The new `cost_onchain_status` field in
  `ListStaticAddressSwaps` says whether the cost is complete, partial (only some
  funding fees are known), unknown (none are known, or the swap spent no
  deposits) or pending (the cost of a deposit hasn't been determined yet, so
  only the known funding fees are included). `loop monitor` doesn't show the
  status: it reports an unknown cost as zero and a partial or pending cost as
  its known part. loopd looks up a deposit's cost within about 10 seconds of
  finding it, and the cost of existing deposits shortly after the first start
  after upgrading. If lnd isn't synced to the chain or the lookup fails, the
  cost stays pending and loopd tries again while it runs. In the rare case that
  a swap takes back deposits that a later swap had already taken, their cost is
  reported for the later swap if it was paid, and otherwise for neither swap. A
  retried swap from before deposits were mapped to swaps can report an unknown
  cost.
  [Issue #1174](https://github.com/lightninglabs/loop/issues/1174)

#### Breaking Changes

* A `--postgres.password` value beginning with `@` is now read as the path to a
  file holding the password rather than as the password itself. Operators whose
  password starts with a literal `@` must double the prefix, as in
  `@@password`, before upgrading. Left unchanged, such a value either stops
  `loopd` from starting or, if a file happens to exist at the implied path,
  connects using that file's contents.
  [Issue #1088](https://github.com/lightninglabs/loop/issues/1088)

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

* Static Address loop-in sweep signing requests and their authenticated
  responses are now processed concurrently, preventing slow connections from
  delaying later responses until the server times them out. Sweep work is
  bounded, and server-provided prevouts are validated before signing.

* Instant Out now attempts to cancel server-side swaps when client
  initialization fails, allowing locked reservations to be released without
  waiting for the server timeout.

* Static Address loop-in requests now fail promptly after the manager exits,
  including shutdown and initialization failures.

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

* A Loop In whose initiation fails now cancels its probe invoice. A probe
  payment that reached the node after the failure was previously held for
  hours, until shortly before its expiry. The probe watcher and failure
  cleanup share successful cancellation results to avoid duplicate requests.
  Cancellation failures are logged, and a failed attempt does not prevent
  the other caller from trying.

#### Maintenance

* Update `github.com/moby/sys/user` to v0.4.1 in `looprpc` and synchronize
  the root module with its required version.
  [PR #1250](https://github.com/lightninglabs/loop/pull/1250)

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
