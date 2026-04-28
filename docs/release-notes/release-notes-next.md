# Loop Client Release Notes

#### New Features

* The `--postgres.password` flag now accepts a `@/path/to/file` syntax that
  reads the password from a file instead of taking it on the command line, so
  the secret is no longer exposed in process listings or shell history. A
  password that begins with a literal `@` is written as `@@password`.
  [Issue #1088](https://github.com/lightninglabs/loop/issues/1088)

#### Breaking Changes

* A `--postgres.password` value beginning with `@` is now read as the path to a
  file holding the password rather than as the password itself. Operators whose
  password starts with a literal `@` must double the prefix, as in
  `@@password`, before upgrading. Left unchanged, such a value either stops
  `loopd` from starting or, if a file happens to exist at the implied path,
  connects using that file's contents.
  [Issue #1088](https://github.com/lightninglabs/loop/issues/1088)

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

* The static address Loop In timeout sweep now spends the exact HTLC output
  confirmed on chain, including server-published backup fee variants.
  Previously it always targeted the standard fee variant, so a confirmed high
  fee or extremely high fee HTLC transaction could not be swept back after the
  HTLC timed out. Sweeps that would leave a dust output are no longer signed.

#### Maintenance

#### Contributors (Alphabetical Order)
