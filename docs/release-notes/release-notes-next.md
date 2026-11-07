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

#### Maintenance

#### Contributors (Alphabetical Order)
