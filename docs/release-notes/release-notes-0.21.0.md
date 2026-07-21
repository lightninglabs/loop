# Loop Client Release Notes

- **Release date:** 2022-12-16
- **Release page:**
  [v0.21.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.21.0-beta)
- **Previous release:** [v0.20.2-beta](release-notes-0.20.2.md)
- **Next release:** [v0.22.0-beta](release-notes-0.22.0.md)

#### New Features

* Autoloop now has a new parameter named `destaddr` which if set to a valid
  bitcoin address will direct all funds from automatically dispatched loop outs
  towards that address.

#### Breaking Changes

* Listing loop-in swaps will not display old, nested segwit swap htlc addresses
  correctly anymore since support for these htlc types is removed from the code
  base.

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- George Tsagkarelis
- sputn1ck
