# Loop Client Release Notes

- **Release date:** 2026-11-05
- **Release page:**
  [v0.35.3-beta](https://github.com/lightninglabs/loop/releases/tag/v0.35.3-beta)
- **Previous release:** [v0.35.2-beta](release-notes-0.35.2.md)
- **Next release:** [v0.35.4-beta](release-notes-0.35.4.md)

#### New Features

* Loop Out accepts bLIP 39 blinded invoices when the server supplies a separate
  payment address for swap RPC authentication. The address is persisted for
  restart and cooperative sweeping. Blinded payments use LND's routing without
  destination-based routing plugins.
  [PR #1255](https://github.com/lightninglabs/loop/pull/1255)

#### Breaking Changes

#### Bug Fixes

* Retain active static-address deposits while a withdrawal publishes and
  records its result, preventing wallet reconciliation from removing spent
  inputs before a slow Neutrino publication returns. Defer new expiry checks
  for these inputs until publication finishes.
  [PR #1176](https://github.com/lightninglabs/loop/pull/1176)

* A sweep batch that cannot construct its transaction now logs the failure and
  retries without stopping `loopd`. New sweeps use another batch until the
  failing one constructs a transaction.
  Presigned co-signer timeouts also retry without stopping other batches.
  [PR #1270](https://github.com/lightninglabs/loop/pull/1270)

* Disable MPTCP listeners by default, restoring plain TCP for `loopd` and
  mitigating the demonstrated CVE-2026-98432 exploit path on vulnerable Linux
  kernels. Existing binaries can apply the same mitigation with
  `GODEBUG=multipathtcp=0` and a restart. A patched kernel is still required to
  fix the underlying vulnerability.
  [PR #1271](https://github.com/lightninglabs/loop/pull/1271)

* Bound the static Loop In risk decision replay cache to prevent unbounded
  memory growth from swap server notifications for unknown swaps. Cached
  decisions retain only a copy of the validated swap hash, without unknown
  protobuf fields or oversized backing buffers.
  [PR #1264](https://github.com/lightninglabs/loop/pull/1264)

#### Maintenance

* Update the regtest environment to use Docker Compose v2 when available,
  falling back to `docker-compose`, and pull the etcd image from
  `bitnamilegacy` so the environment can start.
  [PR #1263](https://github.com/lightninglabs/loop/pull/1263)

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Boris Nagaev
- Slyghtning
- sputn1ck
- ziggie
