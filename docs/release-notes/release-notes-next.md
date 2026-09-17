# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

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

#### Maintenance

#### Contributors (Alphabetical Order)
