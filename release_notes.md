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
* [Enhance](https://github.com/lightninglabs/loop/pull/912) the
  `loop listswaps` command by improving the ability to filter the 
  response.  Use `--start_timestamp_ns` to return only swaps after
  that timestamp. Use `--max_swaps` to limit total swap outputs.
  Paging is enabled using the `next_start_time` field in the response.


#### Breaking Changes

#### Bug Fixes

#### Maintenance
