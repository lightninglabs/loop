# Loop Client Release Notes

- **Release date:** 2024-01-30
- **Release page:**
  [v0.27.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.27.0-beta)
- **Previous release:** [v0.26.6-beta](release-notes-0.26.6.md)
- **Next release:** [v0.27.1-beta](release-notes-0.27.1.md)

#### New Features

* Sweep Batcher: A new sub-system was added that handles all the loopout sweeps.
  Successful loopout HTLCs will no longer be swept back to the wallet via
  individual transactions but will instead form a single transaction that holds
  multiple inputs and pays to a single output. This will significantly reduce
  chain fee costs as it's using less block space by directly consolidating all
  the htlcs to a single address. Loopouts that pay to non-wallet addresses will
  still use individual transactions as their output cannot be mutated.

#### Breaking Changes

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- bitcoin-lightning
- elbandi
- George Tsagkarelis
- GoodDaisy
- Mohamed Awnallah
- shuoer86
- sputn1ck
