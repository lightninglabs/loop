# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* The `postgres.maxconnections` option now limits the number of open
  connections to the Postgres server. It was ignored before, so concurrent
  requests could open more connections than the server accepts. If the option
  isn't set, loopd now opens at most 25 connections.

* Setting a P2PKH or P2SH address or a raw public key as the autoloop
  destination no longer crashes loopd. The address is now decoded for the
  active network, and a raw public key is rejected.

* Autoloop, Instant Out, static address withdrawals and manual HTLC sweeps now
  reject destination addresses of another network, as Loop Out already did. If
  the saved autoloop destination belongs to another network, loopd removes it
  at startup, and autoloop pays to wallet addresses.

#### Maintenance

#### Contributors (Alphabetical Order)
