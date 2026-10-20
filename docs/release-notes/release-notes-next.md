# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* Update `looprpc`'s `github.com/klauspost/compress` dependency to v1.18.7
  and synchronize the root module, including the fix for an out-of-bounds
  read in `s2.NewDict`.

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

#### Maintenance

* `utils.MuSig2Sign` now takes the signers' raw 32-byte private keys and derives their public keys itself, instead of separate private and public key slices. Out-of-range keys are reduced modulo the secp256k1 group order, while keys that reduce to zero and calls with fewer than two signers are rejected. Go callers of this exported helper need to be updated.

#### Contributors (Alphabetical Order)
