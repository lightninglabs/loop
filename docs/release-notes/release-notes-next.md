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

* The static address Loop In timeout sweep now spends the exact HTLC output
  confirmed on chain, including server-published backup fee variants.
  Previously it always targeted the standard fee variant, so a confirmed high
  fee or extremely high fee HTLC transaction could not be swept back after the
  HTLC timed out. Sweeps that would leave a dust output are no longer signed.

* Automatic Loop Out recovery sweeps and `loop out sweephtlc` in database
  mode now wait for the swap's stored `htlc_confs` confirmations before
  signing the sweep. Before, they waited for only one confirmation. Automatic
  recovery sweeps also no longer reveal the preimage when 20 blocks or fewer
  are left before the HTLC expires, or when the HTLC holds less than the swap
  amount, unless the swap already revealed it.

#### Maintenance

#### Contributors (Alphabetical Order)
