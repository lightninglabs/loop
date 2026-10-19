# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* Static address deposits now stay linked to the loop-in that spent them.
  If a loop-in's htlc confirmed after its payment deadline, and other
  loop-ins had selected its deposits in the meantime, the deposits were
  listed under those loop-ins and the first loop-in's on-chain cost was
  missing. loopd also repairs existing deposits at startup.

#### Maintenance
- Update the LoopRPC requirement to the revision used by this release.

#### Contributors (Alphabetical Order)
