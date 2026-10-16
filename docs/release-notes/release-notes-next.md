# Loop Client Release Notes

#### New Features

* Shared asset HTLCs now expose the direction-neutral `AssetHTLCV0` policy for
  both Asset Loop In and Asset Loop Out. `LegacyDepositV0` remains an alias
  with the same policy value and on-chain contract terms.

* Instant Out now validates server invoices against a caller-approved maximum
  swap fee.

#### Breaking Changes

* `SwapKit.CreateHtlcVpkt` now returns a non-interactive funding template that
  matches V1 HTLC addresses for exact-value and change sends. Integrations
  using the original interactive template must explicitly call
  `CreateInteractiveHtlcVpkt` and validate the actual anchor proof.

* Shared deposit kits now require trusted asset genesis metadata, and the group
  key for grouped assets, when creating or validating addresses. Proof-only
  operations continue to accept kits constructed with just the asset ID.

* Shared asset HTLC and deposit kit constructors now require explicit sender
  and receiver internal and script keys. Cooperative spends aggregate only
  internal keys; claims and refunds use script keys. Reusing each party's key
  for both roles preserves existing contracts.

* Shared asset witness builders now require complete, caller-approved virtual
  transfers. Deposit refunds also require the CSV sequence to be set before
  signing any inputs. Construct transfers with `sweep.NewTransfer` to snapshot
  the approved Bitcoin output values, scripts and order before signing.

* Remove the ambiguous deposit `Kit.VerifyProof` method. Use the amount-bound
  `AnchorRootFromProofCommitment` for previously verified proofs, or
  `VerifyProofFile` to verify a complete history and bind the deposit contract.

* Instant Out requests must now set `max_swap_fee_sat`. Requests that omit the
  fee cap are rejected; an explicit zero cap remains valid. Direct users of
  `Manager.NewInstantOut` must pass the fee cap as a required argument.

* Raise the minimum supported LND version to v0.19.0-beta. Since v0.33.3,
  `loopd` could not buy its L402 token on older LND, which rejected the
  payment with `timeout_seconds must be specified`, so a `loopd` without a
  paid token could not use the Loop server.

#### Bug Fixes

* Shared asset sweep signing now requires Bitcoin locktimes and input sequences
  to enforce asset timelocks, preventing anchors from confirming before their
  asset transitions are valid while preserving existing signatures.

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

* Shared asset sweeps validate asset conservation and Bitcoin output commitments,
  including change, passive assets, and the STXO and spender leaves required by
  Taproot Assets v0.8.5, before signing. Fresh inclusion and exclusion proofs
  check the complete Bitcoin transaction, rejecting extra outputs that would
  invalidate asset proofs. New deposit addresses use
  explicit asset V1 and address V1 versions, refund signing preserves existing
  signatures, and returned control blocks no longer expose mutable kit keys.

* Deposit proof verification now requires genesis-rooted histories and rejects
  ownership-only proofs, including those hidden in additional inputs.

* Shared asset HTLC and deposit proof acceptance now requires the proof to
  describe the complete anchor commitment. A funder could otherwise hide an
  extra leaf in the anchor output and block the receiver's claim after the
  receiver paid the invoice or revealed the preimage.

* Shared asset kits now isolate proof and RPC buffers, reject oversized proofs
  and unencodable amounts, and enforce distinct x-only signing keys across all
  sender and receiver roles. Each party may still reuse its own internal key
  as its script key for legacy contracts.

* Shared legacy asset contracts now freeze address, asset, virtual packet,
  commitment and proof versions in one policy definition. Unknown input proof
  semantics are rejected before commitment reconstruction or verification.

* Shared Taproot Asset sweep packets now preserve the destination address
  version and use the required non-interactive split-root layout. Every input's
  group key must match the destination address, so the recipient's Bitcoin
  script matches the address.

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

* Shared asset HTLC helpers and funding templates now return independent NUMS
  keys so caller mutations cannot change later contracts.

* Shared deposit and HTLC addresses are now checked against their complete
  contracts, with output keys derived from trusted asset metadata. RPC responses
  must agree with their encoded addresses; shared validators also accept peer
  addresses.

* Deposit proof acceptance and refund signing now enforce the expected group
  key when trusted asset metadata is supplied. Proof-only kits without that
  metadata remain supported.

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

* Update Taproot Assets to v0.8.5, taprpc to v1.3.3, and the LND dependency to
  v0.21.3-beta. The minimum Go build version is now 1.25.13.

* The Docker image build now verifies that every platform of the image index
  holds binaries for the architecture it advertises, and gives a release its
  tag only once that check has passed.
  [Issue #1211](https://github.com/lightninglabs/loop/issues/1211)

* The regtest environment now runs LND v0.21.0-beta, the version Loop is built
  against, instead of v0.18.5-beta.

#### Contributors (Alphabetical Order)
