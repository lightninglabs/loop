# Loop Client Release Notes

#### New Features

#### Breaking Changes

#### Bug Fixes

* A static Loop In that releases its deposits a second time, at its HTLC
  expiry after its payment deadline, now only releases deposits that are still
  looping in for it. Before, it could move a deposit that another Loop In, a
  withdrawal or a channel open had taken in the meantime back to `Deposited`.

#### Maintenance

#### Contributors (Alphabetical Order)
