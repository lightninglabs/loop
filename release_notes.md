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
* Autoloop can now be configured on a per-peer basis, rather than only on an
  individual channel level. This change allows desired liquidity thresholds 
  to be set for an individual peer, rather than a specific channel, and 
  leverages multi-loop-out to more efficiently manage liquidity. To configure
  peer-level rules, provide the 'setrule' command with the peer's pubkey. 
* Autoloop's fee API has been simplified to allow setting a single percentage
  which will be used to limit total swap fees to a percentage of the amount 
  being swapped, the default budget has been updated to reflect this. Use 
  `loop setparams --feepercent={percentage}` to update this value. This fee 
  setting has been updated to the default for autoloop.
* The default confirmation target for automated loop out swap sweeps has been
  increased to 100 blocks. This change will not affect the time it takes to 
  acquire inbound liquidity, but will decrease the cost of swaps.

#### Breaking Changes

#### Bug Fixes
* The loop dockerfile has been updated to use the `make` command so that the
  latest commit hash of the code being run will be included in `loopd`.
