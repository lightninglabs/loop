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

* `--private` flag is now available on the `loop in` command, meaning users can
  now loop in to private nodes! This was implemented in [#415](https://github.com/lightninglabs/loop/pull/415)
  and has an implementation of LND's hop hints creation feature for the time
  being. The flag is also available in `loop quote in` as an extension.
* `--route_hints` has also been added on the `loop in` and `loop quote in` cli
  commands, and was also include in [#415](https://github.com/lightninglabs/loop/pull/415).
  While the `--private` flag autogenerates routehints to assist the payer (Lightning Labs),
  `--route_hints` allows the user to feed their own crafted versions if they so please.

#### Breaking Changes

#### Bug Fixes

* Fixed issue where loop assumes the mainnet location for lnd.macaroonpath regardless of passed network parameters.

#### Maintenance
