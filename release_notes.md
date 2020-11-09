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

#### NewFeatures

* When requesting a swap, a new `initiator` field can be set on the gRPC/REST
  interface that is appended to the user agent string that is sent to the server
  to give information about Loop usage. The initiator field is meant for user
  interfaces to add their name to give the full picture of the binary used
  (`loopd`, LiT) and the method/interface used for triggering the swap (`loop`
  CLI, autolooper, LiT UI, other 3rd party UI).

#### Breaking Changes

#### Bug Fixes
