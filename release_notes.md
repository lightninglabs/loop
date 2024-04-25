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

In loopd.conf file `maxlsatcost` and `maxlsatfee` were renamed to `maxl402cost`
and `maxl402fee` accordingly. Old versions of the options are still recognized
for backward compatibility, but a deprecation warning is printed. Users are
encouraged to change the options to new names if they have been changed locally.

The path in looprpc "/v1/lsat/tokens" was renamed to "/v1/l402/tokens" and
the corresponding method was renamed from `GetLsatTokens` to `GetL402Tokens`.
New `loop` binary won't work with old `loopd`, because `loop listauth` is now
calling `GetL402Tokens` method, which does not exist in previous `loopd` binary.
Old `loop` binary works with new `loopd`, since `loopd` provides a wrapper for
`GetLsatTokens` which calls `GetL402Tokens`; the wrapper logs a warning and
it will be removed in a couple of releases. HTTP endpoint "/v1/l402/tokens" is
now an additional binding for API "/v1/lsat/tokens", so it still works.

#### Bug Fixes

#### Maintenance
