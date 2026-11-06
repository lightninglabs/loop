# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* The `postgres.maxconnections` option now limits the number of open
  connections to the Postgres server. It was ignored before, so concurrent
  requests could open more connections than the server accepts. If the option
  isn't set, loopd now opens at most 25 connections.

* A static address loop-in whose htlc had already spent its deposits no
  longer gives up the deposits when the server rejects the confirmation risk
  wait, also when the rejection is handled again after a restart. The client
  keeps the deposits locked and sweeps the htlc timeout path once the htlc
  times out.

#### Maintenance

#### Contributors (Alphabetical Order)
