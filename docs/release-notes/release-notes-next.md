# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* Validate each selected static deposit's CSV expiry against the actual HTLC
  expiry before signing.

* Preserve pending loop-in ownership of deposits during expiry and restart
  recovery. Release only deposits still owned by the swap, after a positive
  unspent-input or confirmed-conflict check rules out a confirmed HTLC.
  Recover abandoned initialization locks at startup, retry temporary wallet
  lookup failures, and finish timed-out swaps after their deposits are reused.
  Check live swap claims before releasing deposits with stale legacy mappings.
  New swaps retain unconfirmed funding transactions so a wallet-visible mined
  replacement can prove that the original HTLC cannot confirm after restart.
  Missing history alone does not release deposits; older swaps without a funding
  snapshot continue monitoring for recovery.

* Follow replacement static deposit expiry sweeps after a chain reorganization
  instead of waiting for the old transaction until loopd restarts.

* Static Address loop-in sweep requests are now rejected unless the server
  provides exactly one prevout for every sweep input. A duplicate or missing
  prevout previously crashed `loopd` while computing the sweep signature
  hashes.

#### Maintenance

- Update the LoopRPC requirement to the revision used by this release.

#### Contributors (Alphabetical Order)
