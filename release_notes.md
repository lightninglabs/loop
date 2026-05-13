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

#### Breaking Changes

#### Bug Fixes

* [Static Address Withdrawals: Add retry and reorg handling for withdrawal
  confirmations](https://github.com/lightninglabs/loop/issues/1087).
  Previously, if `RegisterSpendNtfn` or `RegisterConfirmationsNtfn` failed,
  the withdrawal monitoring would silently stop. Additionally, reorgs were not
  handled, and state was not persisted before monitoring setup, causing
  recovery failures on restart. This fix adds automatic retry on next block
  for registration failures, proper reorg detection via `WithReOrgChan`, and
  reorders state persistence to occur before monitoring setup.

#### Maintenance
