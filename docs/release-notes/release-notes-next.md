# Loop Client Release Notes

#### New Features

#### Breaking Changes

* `loop static openchannel` now defaults to anchor channels. Remove
  `--channel_type=tweakless` from scripts to use the new default. RPC callers
  that specify `STATIC_REMOTE_KEY` must omit the commitment type or select a
  supported type. Opening a public anchor channel requires an on-chain
  reserve in the funding node's lnd wallet.

#### Bug Fixes

* Reject static Loop In server responses with missing or extra HTLC nonces
  before signing, preventing a client crash from an incomplete nonce list.

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

* The static address Loop In timeout sweep now spends the exact HTLC output
  confirmed on chain, including server-published backup fee variants.
  Previously it always targeted the standard fee variant, so a confirmed high
  fee or extremely high fee HTLC transaction could not be swept back after the
  HTLC timed out. Sweeps that would leave a dust output are no longer signed.

#### Maintenance

#### Contributors (Alphabetical Order)
