# Loop Client Release Notes

#### New Features

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.

#### Breaking Changes

#### Bug Fixes

* Disable MPTCP listeners by default, restoring plain TCP for `loopd` and
  mitigating the demonstrated CVE-2026-98432 exploit path on vulnerable Linux
  kernels. Existing binaries can apply the same mitigation with
  `GODEBUG=multipathtcp=0` and a restart. A patched kernel is still required to
  fix the underlying vulnerability.

* Bound the static Loop In risk decision replay cache to prevent unbounded
  memory growth from swap server notifications for unknown swaps. Cached
  decisions retain only a copy of the validated swap hash, without unknown
  protobuf fields or oversized backing buffers.

#### Maintenance
- Update the LoopRPC requirement to the revision used by this release.

#### Contributors (Alphabetical Order)
