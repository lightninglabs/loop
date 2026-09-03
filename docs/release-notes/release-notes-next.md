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

* Instant Out requests must now set `max_swap_fee_sat`. Requests that omit the
  fee cap are rejected; an explicit zero cap remains valid. Direct users of
  `Manager.NewInstantOut` must pass the fee cap as a required argument.

* Calling `NewStaticAddress` without `send_coins_request.addr` now derives and
  returns a fresh receive address instead of reusing the address associated
  with the client's L402. Integrations must not assume that repeated calls are
  idempotent or return the same address. The RPC now requires the
  `swap:execute` permission instead of `swap:read`, including for address-only
  calls. Operators using custom scoped macaroons must rebake them accordingly.
  The deprecated `StaticAddressSummaryResponse.static_address` field remains
  the legacy/root address for compatibility and must not be treated as the
  current receive address; call `NewStaticAddress` to derive a fresh one.
  [PR #1218](https://github.com/lightninglabs/loop/pull/1218)

#### Bug Fixes

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
  and seed creation can recover from a failed wallet import.

* The `NewStaticAddress` RPC can fund a requested existing static address by
  resolving it directly through the active script index. Wallet-import errors
  are ignored only when they identify the exact script that lnd already
  watches.

* Static Address loop-in sweep requests are now rejected unless the server
  provides exactly one prevout for every sweep input. A duplicate or missing
  prevout previously crashed `loopd` while computing the sweep signature
  hashes.

#### Maintenance

* The Docker image build now verifies that every platform of the image index
  holds binaries for the architecture it advertises, and gives a release its
  tag only once that check has passed.
  [Issue #1211](https://github.com/lightninglabs/loop/issues/1211)

#### Contributors (Alphabetical Order)
