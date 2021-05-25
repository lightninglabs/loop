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
- The loopd client reports off-chain routing failures for loop out swaps if 
  it cannot find a route to the server for the swap's prepay or invoice payment.
  This allows the server to release accepted invoices, if there are any, 
  earlier, reducing the amount of time that funds are held off-chain. If the  
  swap failed on one of the loop server's channels, it will report failure 
  location of its off-chain failure. If the failure occurred outside of the 
  loop server's infrastructure, a generic failure will be used so that no 
  information about the client's position in the network is leaked.

#### Breaking Changes

#### Bug Fixes
