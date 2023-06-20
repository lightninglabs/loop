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
* Deprecated boltdb, we now support both sqlite and postgres. On first startup, loop will automatically migrate your database to sqlite. If you want to use postgres, you can set the `--databasebackend` flag to `postgres` and set the `--postgres.host`, `--postgres.port`, `--postgres.user`, `--postgres.password` and `--postgres.dbname` flags to connect to your postgres instance. Your boltdb file will be saved as a backup in the loop directory. NOTE: we're currently only supporting migrating once from boltdb to a sql backend. A manual migration between postgres and sqlite will be supported in the future.
#### Breaking Changes

#### Bug Fixes

#### Maintenance
