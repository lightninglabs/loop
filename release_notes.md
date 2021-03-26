# Loop Client Release Notes
This file tracks release notes for the loop client. 

### Developers: 
* When new features are added to the repo, a short description of the feature should be added under the "Next Release" heading.
* This should be done in the same PR as the change so that our release notes stay in sync!

### Release Manager: 
* All of the items under the "Next Release" heading should be included in the release notes.
* As part of the PR that bumps the client version, cut everything below the 'Next Release' heading. 
* These notes can either be pasted in a temporary doc, or you can get them from the PR diff once it is merged. 
* The notes are just a guideline as to the changes that have been made since the last release, they can be updated.
* Once the version bump PR is merged and tagged, add the release notes to the tag on GitHub.

## Next release

#### New Features
* Two new flags, `--max_sweep_fee` and `--max_sweep_fee_percent`, are added to
  `loop out`. Users now can specify the max on-chain fee to be paid when
  sweeping the HTLCs. `max_sweep_fee` specifies the max fee can be used in
  satoshis, default to the prepay amount. `max_sweep_fee_percent` specifies a
  portion based on the total swap amount. Note that these flags cannot be used
  together. When neither is specified, the default value of `max_sweep_fee` will
  be used. Be cautious when setting the max fee as a low value might cause
  forfeiting the prepay.

#### Breaking Changes

#### Bug Fixes
