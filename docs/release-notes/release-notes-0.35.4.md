# Loop Client Release Notes

- **Release date:** Unreleased
- **Release page:**
  [v0.35.4-beta](https://github.com/lightninglabs/loop/releases/tag/v0.35.4-beta)
- **Previous release:** [v0.35.3-beta](release-notes-0.35.3.md)
- **Next release:** [Next release](release-notes-next.md)

#### New Features

#### Breaking Changes

#### Bug Fixes

* Reject incomplete static-address loop-in signing responses without crashing
  the daemon.
  [PR #1282](https://github.com/lightninglabs/loop/pull/1282)

* Reject invalid static-address loop-in fee quotes before creating invoices.
  [PR #1281](https://github.com/lightninglabs/loop/pull/1281)

* Reject invalid loop-in fee quotes before creating invoices.
  [PR #1284](https://github.com/lightninglabs/loop/pull/1284)

* The `postgres.maxconnections` option now limits the number of open
  connections to the Postgres server. It was ignored before, so concurrent
  requests could open more connections than the server accepts. If the option
  isn't set, loopd now opens at most 25 connections. Migration connections
  are returned to the pool, and waiting for the migration lock respects the
  configured timeout.
  [PR #1262](https://github.com/lightninglabs/loop/pull/1262)

* A static address loop-in whose HTLC had already spent its deposits no
  longer gives up the deposits when the server rejects the confirmation risk
  wait, also when the rejection is handled again after a restart. The client
  keeps the deposits locked and sweeps the HTLC timeout path once the HTLC
  times out. Without an observed HTLC confirmation, the client only keeps the
  deposits if lnd's wallet shows that the HTLC transaction spent them.
  Otherwise, for example when a deposit's funding transaction was replaced
  and lnd doesn't list an HTLC transaction, the rejection fails the swap and
  releases the deposits, as in v0.35.3.
  [PR #1278](https://github.com/lightninglabs/loop/pull/1278)
  [PR #1288](https://github.com/lightninglabs/loop/pull/1288)

* Setting a P2PKH or P2SH address or a raw public key as the autoloop
  destination no longer crashes loopd. The address is now decoded for the
  active network, and a raw public key is rejected. Instant Out also rejects
  raw public keys as destinations, matching Loop Out.
  [PR #1280](https://github.com/lightninglabs/loop/pull/1280)

* Autoloop, Instant Out, static address withdrawals and manual HTLC sweeps now
  reject destination addresses of another network, as Loop Out already did. If
  the saved autoloop destination belongs to another network, loopd removes it
  at startup, and autoloop pays to wallet addresses.
  [PR #1280](https://github.com/lightninglabs/loop/pull/1280)

#### Maintenance

#### Contributors (Alphabetical Order)

- Boris Nagaev
- claravanstaden
- Slyghtning
