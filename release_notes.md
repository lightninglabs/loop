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
* Loop in functionality has been added to AutoLoop. This feature can be enabled
  to acquire outgoing capacity on your node automatically, using 
  `loop setrule --type=in`. At present, autoloop can only be set to loop out 
  *or* loop in, and cannot manage liquidity in both directions.

#### Breaking Changes

#### Bug Fixes

* Close local databases when loopd daemon is stopped programmatically.
* Fix hop hint selection for probed private loop-in.

#### Maintenance
