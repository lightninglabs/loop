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
* Static address loop-ins now abort if the server sets an HTLC expiry that
  leaves less than 50 blocks before a selected deposit expires. A deposit that
  expired first left the swap for its expiry sweep. If the HTLC transaction
  had already spent the deposit, that sweep could never confirm and the HTLC
  output was never swept back.

#### Maintenance

#### Contributors (Alphabetical Order)
