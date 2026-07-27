# Loop Client Release Notes

- **Release date:** 2021-03-26
- **Release page:**
  [v0.12.1-beta](https://github.com/lightninglabs/loop/releases/tag/v0.12.1-beta)
- **Previous release:** [v0.12.0-beta](release-notes-0.12.0.md)
- **Next release:** [v0.12.2-beta](release-notes-0.12.2.md)

#### New Features

* A new flag, `--verbose`, or `-v`, is added to `loop in`, `loop out` and
  `loop quote`. Responses from these commands are also updated to provide more
  verbose info, giving users a more intuitive view about money paid on/off-chain
  and fees incurred. Use `loop in -v`, `loop out -v`, `loop quote in -v` or
  `loop quote out -v` to view the details.
* A stripped down version of the Loop server is now provided as a
  [Docker image](https://hub.docker.com/r/lightninglabs/loopserver). A quick
  start script and example `docker-compose` environment as well as
  [documentation on how to use the `regtest` Loop server](https://github.com/lightninglabs/loop/blob/master/regtest/README.md)
  was added too.

#### Breaking Changes

#### Bug Fixes

* A bug that would not list autoloop rules set on a per-peer basis when they
  were excluded due to insufficient budget, or the number of swaps in flight has
  been corrected. These rules will now be included in the output of
  `suggestswaps` with other autoloop peer rules.

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Carla Kirk-Cohen
- Elle Mouton
- Justin O'Brien
- Oliver Gugger
- Yong Yu
