# Loop Client Release Notes

- **Release date:** 2019-07-25
- **Release page:**
  [v0.2.1-alpha](https://github.com/lightninglabs/loop/releases/tag/v0.2.1-alpha)
- **Previous release:** [v0.2.0-alpha](release-notes-0.2.0.md)
- **Next release:** [v0.2.2-alpha](release-notes-0.2.2.md)

#### New Features

This release includes a new option to specify the sweep confirmation target for
Loop Out.

```
--conf_target=block_count
```

This will delay the finality of the swap in favor of a potential reduction in
chain fee expenditure.

If you are using this feature, please note that it does become increasingly
important that you do not allow `loopd` to terminate before the swap is final,
otherwise the timeout pathway may activate in the HTLC.

Because of the finality problem, the target will only be used at the start of
the swap preimage reveal time period and is not a guaranteed final sweep fee
rate.

#### Breaking Changes

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Wilmer Paulino
