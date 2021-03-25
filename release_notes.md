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
* A new flag, `--verbose`, or `-v`, is added to `loop in`, `loop out` and
 `loop quote`. Responses from these commands are also updated to provide more
  verbose info, giving users a more intuitive view about money paid
  on/off-chain and fees incurred. Use `loop in -v`, `loop out -v`,
  `loop quote in -v` or `loop quote out -v` to view the details.
* A stripped down version of the Loop server is now provided as a
  [Docker image](https://hub.docker.com/r/lightninglabs/loopserver). A quick
  start script and example `docker-compose` environment as well as
  [documentation on how to use the `regtest` Loop server](https://github.com/lightninglabs/loop/blob/master/regtest/README.md)
  was added too.

#### Breaking Changes

#### Bug Fixes
* A bug that would not list autoloop rules set on a per-peer basis when they 
  were excluded due to insufficient budget, or the number of swaps in flight
  has been corrected. These rules will now be included in the output of 
  `suggestswaps` with other autoloop peer rules.
