# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* Reject invalid static-address loop-in fee quotes before creating invoices.

* A static address deposit no longer misses its expiry when the last block
  arrives while lnd's wallet tip is still changing. Before, the deposit
  manager skipped that block and waited for the next one before checking
  expiries again.

* The `postgres.maxconnections` option now limits the number of open
  connections to the Postgres server. It was ignored before, so concurrent
  requests could open more connections than the server accepts. If the option
  isn't set, loopd now opens at most 25 connections.

#### Maintenance

#### Contributors (Alphabetical Order)
