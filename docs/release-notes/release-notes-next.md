# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* The `postgres.maxconnections` option now limits the number of open
  connections to the Postgres server. It was ignored before, so concurrent
  requests could open more connections than the server accepts. If the option
  isn't set, loopd now opens at most 25 connections.

#### Maintenance

#### Contributors (Alphabetical Order)
