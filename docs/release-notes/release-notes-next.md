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
* A static address loop-in no longer takes a confirmed transaction that pays to
  its htlc as the htlc transaction unless that transaction also spends the
  swap's deposits. Before, the server could confirm a dust output to the htlc
  script and lock the deposits in a timeout sweep that could never succeed.
  The htlc transaction is then found from the spend of the deposits, and at
  the htlc expiry the deposits are only released if lnd's wallet lists no
  htlc transaction that spent them.

#### Maintenance

#### Contributors (Alphabetical Order)
