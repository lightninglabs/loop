# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* A static Loop In that releases its deposits a second time after its payment
  deadline, at its HTLC expiry or when the server rejects the swap, now only
  releases deposits that are still looping in for it. Before, it could move a
  deposit that another Loop In, a withdrawal or a channel open had taken in the
  meantime back to `Deposited`, and retried forever once the deposit was spent.
  A static Loop In also no longer locks a deposit that another Loop In is
  already using, and a failed lock no longer resets deposits that a withdrawal
  or a channel open has taken.
* Automatic recovery of a confirmed Loop Out HTLC no longer sweeps the HTLC
  of a swap that is still running in the daemon. The HTLC is left to the
  running swap, which sweeps it and records that the preimage was revealed,
  or has decided not to reveal the preimage. Before, recovery could publish
  the preimage without recording it, so after a restart the swap could be
  abandoned even though its preimage was already public.

#### Maintenance

#### Contributors (Alphabetical Order)
