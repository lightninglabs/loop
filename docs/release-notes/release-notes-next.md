# Loop Client Release Notes

#### New Features

* Experimental clients can quote and purchase an on-chain asset reservation,
  verify its funding proof against the agreed CSV contract, and resume a
  pending purchase after restarting. Buyers can request an exact issuance or
  a fungible asset group and verify both quoted issuances before payment.
  A reorg that invalidates a delivered proof withdraws its readiness and
  advertised deadlines. Delivery uses the execution cutoff without an
  additional minimum usable-window term.

* Share asset deposit, reservation and CSV/CLTV HTLC contracts through
  `swapkit`, with transfer, signing, proof and recovery helpers backed by
  `github.com/lightninglabs/tap-sdk` v0.1.1 and Taproot Assets v0.8.8. New on-chain
  asset swaps require tapd v0.8.8 or later. Shared helpers also authenticate
  group issuance evidence and register issuance metadata before verification.

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

* Harden the shared `swapkit` adapter against hostile peer input: bind a
  recovery fallback to the authenticated deposit before co-signing, verify a
  caller-provided asset witness before declaring it, recognize annex spends,
  and report a reorg during outcome verification as retryable.

#### Maintenance

#### Contributors (Alphabetical Order)
