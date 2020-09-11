# Loop Client Release Notes
This file tracks release notes for the loop client. 

### Developers: 
* When new features are added to the repo, a short description of the feature should be added under the "Next Release" heading.
* This should be done in the same PR as the change so that our release notes stay in sync!

### Release Manager: 
* All of the items under the "Next Release" heading should be included in the release notes.
* As part of the PR that bumps the client version, the "Next Release" heading should be replaced with the release version including the changes.

## Next Release

#### New Features

#### Breaking Changes

* Macaroon authentication has been enabled for the `loopd` gRPC and REST
  connections. This makes it possible for the loop API to be exposed safely over
  the internet as unauthorized access is now prevented.
  
  The daemon will write a default `loop.macaroon` in its main directory. For
  mainnet this file will be picked up automatically by the `loop` CLI tool. For
  testnet you need to specify the `--network=testnet` flag.
  [More information about TLS and macaroons.](README.md#authentication-and-transport-security)

#### Bug Fixes