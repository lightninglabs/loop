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
* A static Loop In no longer releases its deposits when pushing its HTLC
  signatures to the server fails or when loopd restarts during signing. The
  server may have received the signatures and can publish the HTLC, so the
  swap keeps monitoring it and the deposits stay locked until the swap's
  payment deadline, or the HTLC expiry at the latest.

#### Maintenance

#### Contributors (Alphabetical Order)
