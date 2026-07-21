# Loop Client Release Notes

- **Release date:** 2020-10-13
- **Release page:**
  [v0.10.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.10.0-beta)
- **Previous release:** [v0.9.0-beta](release-notes-0.9.0.md)
- **Next release:** [v0.11.0-beta](release-notes-0.11.0.md)

#### New Features

* Multi-path payment has been enabled for Loop In. This means that it is now
  possible to replenish multiple channels via a single Loop In request and a
  single on-chain htlc. This has to potential to greatly reduce chain fee costs.
  Note that it is not yet possible to select specific peers to loop in through.
* The daemon now sends a user agent string with each swap. This allows
  developers to identify their fork or custom implementation of the loop client.

**Updated Swap Suggestions**

* The swap suggestions endpoint has been updated to be fee-aware. Swaps that
  exceed the fee limits set by the liquidity manager will no longer be suggested
  (see `getparams` for the current limits, and use `setparams` to update these
  values).
* Swap suggestions are now aware of ongoing and previously failed swaps. They
  will not suggest swaps for channels that are currently being utilized for
  swaps, and will not suggest any swaps if a swap that is not limited to a
  specific peer or channel is ongoing. If a channel was part of a failed swap
  within the last 24H, it will be excluded from our swap suggestions (this value
  is configurable).
* The `debug` logging level is recommended if using this feature.

#### Breaking Changes

* Macaroon authentication has been enabled for the `loopd` gRPC and REST
  connections. This makes it possible for the loop API to be exposed safely over
  the internet as unauthorized access is now prevented.

The daemon will write a default `loop.macaroon` in its main directory. For
mainnet this file will be picked up automatically by the `loop` CLI tool. For
testnet you need to specify the `--network=testnet` flag.
[More information about TLS and macaroons.](../../README.md#authentication-and-transport-security)

* The `setparm` loopcli endpoint is renamed to `setrule` because this endpoint
  is only used for setting liqudity rules (parameters can be set using the new
  `setparams` endpoint).

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Carla Kirk-Cohen
- Joost Jager
- Oliver Gugger
