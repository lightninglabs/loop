# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* A static address loop-in whose htlc had already spent its deposits no
  longer gives up the deposits when the server rejects the confirmation risk
  wait, also when the rejection is handled again after a restart. The client
  keeps the deposits locked and sweeps the htlc timeout path once the htlc
  times out.

#### Maintenance

#### Contributors (Alphabetical Order)
