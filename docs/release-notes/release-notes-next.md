# Loop Client Release Notes

#### New Features

#### Breaking Changes

* `loop static deposit` now funds a fresh address from loopd's connected lnd
  wallet. Pass `--amt` or `--sweepall`. The new `FundStaticAddress` RPC uses
  the existing `swap:execute` and `loop:in` permissions. Loopd's lnd macaroon
  must authorize `SendCoins` (`onchain:write`). Scripts that only need an
  address can use `loop static new`. The funding RPC limits fee rates to
  100 sat/vbyte and checks for a deposit of at least 15,000 sat. A sweep
  checks the expected amount after fees and the channel reserve.

#### Bug Fixes

#### Maintenance

* Update the OpenTelemetry Go SDK from v1.45.0 to v1.47.0 in Loop and
  `looprpc`.

#### Contributors (Alphabetical Order)
