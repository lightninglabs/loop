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

* The loop client now labels all its on-chain transactions to make them easily
  identifiable in `lnd`'s `listchaintxns` output.
  
##### Introducing Autoloop
* This release includes support for opt-in automatic dispatch of loop out swaps, 
  based on the output of the `Suggestions` endpoint. 
* To enable the autolooper, the following command can be used: 
  `loop setparams --autoout=true --autobudget={budget in sats} --budgetstart={start time for budget}`
* Automatically dispatched swaps are identified in the output of the 
  `ListSwaps` with the label `[reserved]: autoloop-out`. 
* If autoloop is not enabled, the client will log the actions that the 
  autolooper would have taken if it was enabled, and the `Suggestions` endpoint
  can be used to view the exact set of swaps that the autolooper would make if 
  enabled. 
  
#### Breaking Changes

#### Bug Fixes
