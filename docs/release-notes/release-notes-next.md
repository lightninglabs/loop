# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* Reject incomplete static-address loop-in signing responses without crashing
  the daemon.

* Reject invalid static-address loop-in fee quotes before creating invoices.

* Reject invalid loop-in fee quotes before creating invoices.

* The `postgres.maxconnections` option now limits the number of open
  connections to the Postgres server. It was ignored before, so concurrent
  requests could open more connections than the server accepts. If the option
  isn't set, loopd now opens at most 25 connections.

* A static address loop-in whose htlc had already spent its deposits no
  longer gives up the deposits when the server rejects the confirmation risk
  wait, also when the rejection is handled again after a restart. The client
  keeps the deposits locked and sweeps the htlc timeout path once the htlc
  times out.

* Setting a P2PKH or P2SH address or a raw public key as the autoloop
  destination no longer crashes loopd. The address is now decoded for the
  active network, and a raw public key is rejected.

#### Maintenance

#### Contributors (Alphabetical Order)
