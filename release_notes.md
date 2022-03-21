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

* Loop client now supports optional routing plugins to improve off-chain payment
  reliability. One such plugin that the client implemenets will gradually prefer
  increasingly more expensive routes in case payments using cheap routes time out.
  Note that with this addition the minimum required LND version is LND 0.14.2-beta.

#### Breaking Changes

#### Bug Fixes

* Loop now supports being hooked up to a remote signing pair of `lnd` nodes,
  as long as `lnd` is `v0.14.3-beta` or later.

#### Maintenance
