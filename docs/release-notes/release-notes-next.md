# Loop Client Release Notes

#### New Features

* Share asset deposit, reservation and CSV/CLTV HTLC contracts through
  `swapkit`, with transfer, signing, proof and recovery helpers backed by
  `github.com/lightninglabs/tap-sdk` v0.1.1 and Taproot Assets v0.8.8. New on-chain
  asset swaps require tapd v0.8.8 or later.

#### Breaking Changes

#### Bug Fixes

* The `postgres.maxconnections` option now limits the number of open
  connections to the Postgres server. It was ignored before, so concurrent
  requests could open more connections than the server accepts. If the option
  isn't set, loopd now opens at most 25 connections.

#### Maintenance

* Define the experimental asset reservation RPC contract. Service registration
  and real-node adapters remain disabled.
  Describe public reservation status and owned get/list queries.
  Clarify the receiving node and conversion peer in reservation quotes.

#### Contributors (Alphabetical Order)
