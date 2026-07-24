# Loop Client Release Notes

- **Release date:** 2020-10-27
- **Release page:**
  [v0.11.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.11.0-beta)
- **Previous release:** [v0.10.0-beta](release-notes-0.10.0.md)
- **Next release:** [v0.11.1-beta](release-notes-0.11.1.md)

#### New Features

* The loop client now labels all its on-chain transactions to make them easily
  identifiable in `lnd` 's `listchaintxns` output.

**Introducing Autoloop**

* This release includes support for opt-in automatic dispatch of loop out swaps,
  based on the output of the `Suggestions` endpoint.
* To enable the autolooper, the following command can be used:
  `loop setparams --autoout=true --autobudget={budget in sats} --budgetstart={start time for budget}`
* Automatically dispatched swaps are identified in the output of the `ListSwaps`
  with the label `[reserved]: autoloop-out`.
* If autoloop is not enabled, the client will log the actions that the
  autolooper would have taken if it was enabled, and the `Suggestions` endpoint
  can be used to view the exact set of swaps that the autolooper would make if
  enabled.

#### Breaking Changes

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Carla Kirk-Cohen
- Oliver Gugger
