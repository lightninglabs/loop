# Loop Client Release Notes
This file tracks release notes for the loop client. 

### Developers: 
* When new features are added to the repo, a short description of the feature should be added under the "Next Release" heading.
* This should be done in the same PR as the change so that our release notes stay in sync!

### Release Manager: 
* All of the items under the "Next Release" heading should be included in the release notes.
* As part of the PR that bumps the client version, the "Next Release" heading should be replaced with the release version including the changes.

## 0.10.0

#### New Features

* Multi-path payment has been enabled for Loop In. This means that it is now possible
  to replenish multiple channels via a single Loop In request and a single on-chain htlc.
  This has to potential to greatly reduce chain fee costs. Note that it is not yet possible
  to select specific peers to loop in through.
* The daemon now sends a user agent string with each swap. This allows
  developers to identify their fork or custom implementation of the loop client.

##### Updated Swap Suggestions
* The swap suggestions endpoint has been updated to be fee-aware. Swaps that 
  exceed the fee limits set by the liquidity manager will no longer be 
  suggested (see `getParams` for the current limits, and use `setParams` to 
  update these values). 
* Swap suggestions are now aware of ongoing and previously failed swaps. They 
  will not suggest swaps for channels that are currently being utilized for 
  swaps, and will not suggest any swaps if a swap that is not limited to a 
  specific peer or channel is ongoing. If a channel was part of a failed swap 
  within the last 24H, it will be excluded from our swap suggestions (this 
  value is configurable).
* The `debug` logging level is recommended if using this feature.

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

* Macaroon authentication has been enabled for the `loopd` gRPC and REST
  connections. This makes it possible for the loop API to be exposed safely over
  the internet as unauthorized access is now prevented.
  
  The daemon will write a default `loop.macaroon` in its main directory. For
  mainnet this file will be picked up automatically by the `loop` CLI tool. For
  testnet you need to specify the `--network=testnet` flag.
  [More information about TLS and macaroons.](README.md#authentication-and-transport-security)

* The `setparm` loopcli endpoint is renamed to `setrule` because this endpoint 
  is only used for setting liqudity rules (parameters can be set using the new 
  `setparams` endpoint).

#### Bug Fixes