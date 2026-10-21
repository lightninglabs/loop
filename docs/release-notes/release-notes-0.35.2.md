# Loop Client Release Notes

- **Release date:** 2026-10-21
- **Release page:**
  [v0.35.2-beta](https://github.com/lightninglabs/loop/releases/tag/v0.35.2-beta)
- **Previous release:** [v0.35.1-beta](release-notes-0.35.1.md)
- **Next release:** [Next release](release-notes-next.md)

#### New Features

#### Breaking Changes

#### Bug Fixes

* Update `github.com/klauspost/compress` to v1.18.7 in the root and
  `looprpc` modules, including the fix for an out-of-bounds read in
  `s2.NewDict`.

* A MuSig2 Loop In no longer reveals the internal key of its HTLC to the
  server when the swap invoice is canceled. The key is only shared once the
  invoice is paid.

* A Loop In now cancels its swap invoice before it refunds an expired HTLC,
  and refunds only once lnd confirmed that the invoice can no longer be
  paid. Previously the invoice stayed payable until the refund confirmed, so
  a late payment could settle while the HTLC was being refunded.

* A static address loop-in whose HTLC was published without payment now
  refunds the HTLC after it expires. The swap could get stuck retrying a
  deposit state transition forever and never publish the refund.

* Loop-in quotes for selected static address deposits now resolve the
  deposits in the set of currently tracked deposits instead of the rendered
  deposit listing. Every selected deposit must be unique, tracked and in the
  deposited state.

* `ListUnspentDeposits` now refreshes the static address deposits before it
  lists the wallet outputs, and only lists deposits that are currently
  tracked as deposited. An output that disappeared during the refresh could
  previously still be listed, with a stale confirmation count.

* Static address loop-ins now store their times in UTC. On Postgres outside
  UTC, these times read back shifted, which moved the payment deadline:
  later east of UTC, and already over west of UTC, so the invoice was
  cancelled straight away. Times stored before upgrading keep their local
  time, so a swap's state and `loop monitor` now follow the order in which
  its updates were stored instead of their times.

#### Maintenance

* `utils.MuSig2Sign` now takes the signers' raw 32-byte private keys and derives their public keys itself, instead of separate private and public key slices. Out-of-range keys are reduced modulo the secp256k1 group order, while keys that reduce to zero and calls with fewer than two signers are rejected. Go callers of this exported helper need to be updated.

* Raise the minimum Go version to 1.27.3 and move the build and release
  toolchain to match. Go 1.27.3 carries the fix for CVE-2026-111943, which
  distrusts a set of intermediate certificates in `crypto/x509`. Release
  binaries are statically linked, so they only pick up the new trust
  decision when rebuilt with a patched toolchain. Update golangci-lint to
  v2.15.0 for Go 1.27.3 support and regenerate the protobuf bindings.

#### Contributors (Alphabetical Order)

- Boris Nagaev
- claravanstaden
- claudell
- dependabot[bot]
- Slyghtning
