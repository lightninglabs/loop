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

##### Autoloop Swap Size
* Autoloop can now be configured with custom swap size limits. Previously,
autoloop would use the minimum/maximum swap amount set by the server (exposed 
by the `loop terms` command) to decide on swap size. 
* Setting a custom minimum swap amount is particularly useful for clients that 
would like to perform fewer, larger swaps to save on fees. The trade-off when 
setting a large minimum amount is that autoloop will wait until your channel is 
at least the minimum amount below its incoming threshold amount before executing
a swap, which may result in channels staying under the threshold for longer. 
* These values can be set using the following command: `loop setparams --minamt={minimum in sats} --maxamt={maximum in sats}`.
* The values set must fall within the limits set by the loop server. 

#### Breaking Changes

#### Bug Fixes
