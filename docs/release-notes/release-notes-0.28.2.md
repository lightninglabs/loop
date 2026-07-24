# Loop Client Release Notes

- **Release date:** 2024-05-25
- **Release page:**
  [v0.28.2-beta](https://github.com/lightninglabs/loop/releases/tag/v0.28.2-beta)
- **Previous release:** [v0.28.1-beta](release-notes-0.28.1.md)
- **Next release:** [v0.28.3-beta](release-notes-0.28.3.md)

#### New Features

#### Breaking Changes

In loopd.conf file `maxlsatcost` and `maxlsatfee` were renamed to `maxl402cost`
and `maxl402fee` accordingly. Old versions of the options are still recognized
for backward compatibility, but a deprecation warning is printed. Users are
encouraged to change the options to new names if they have been changed locally.

The path in looprpc "/v1/lsat/tokens" was renamed to "/v1/l402/tokens" and the
corresponding method was renamed from `GetLsatTokens` to `GetL402Tokens`. New
`loop` binary won't work with old `loopd`, because `loop listauth` is now
calling `GetL402Tokens` method, which does not exist in previous `loopd` binary.
Old `loop` binary works with new `loopd`, since `loopd` provides a wrapper for
`GetLsatTokens` which calls `GetL402Tokens`; the wrapper logs a warning and it
will be removed in a couple of releases. HTTP endpoint "/v1/l402/tokens" is now
an additional binding for API "/v1/lsat/tokens", so it still works.

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Boris Nagaev
- Slyghtning
