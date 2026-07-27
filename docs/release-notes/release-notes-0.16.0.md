# Loop Client Release Notes

- **Release date:** 2021-12-14
- **Release page:**
  [v0.16.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.16.0-beta)
- **Previous release:** [v0.15.1-beta](release-notes-0.15.1.md)
- **Next release:** [v0.17.0-beta](release-notes-0.17.0.md)

#### New Features

* `--private` flag is now available on the `loop in` command, meaning users can
  now loop in to private nodes! This was implemented in
  [#415](https://github.com/lightninglabs/loop/pull/415) and has an
  implementation of LND's hop hints creation feature for the time being. The
  flag is also available in `loop quote in` as an extension.
* `--route_hints` has also been added on the `loop in` and `loop quote in` cli
  commands, and was also include in
  [#415](https://github.com/lightninglabs/loop/pull/415). While the `--private`
  flag autogenerates routehints to assist the payer (Lightning Labs),
  `--route_hints` allows the user to feed their own crafted versions if they so
  please.

#### Breaking Changes

#### Bug Fixes

* Fixed issue where loop assumes the mainnet location for lnd.macaroonpath
  regardless of passed network parameters.

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Carla Kirk-Cohen
- Harsha Goli
- Martin Habovštiak
- Oliver Gugger
- Turtle
- Yong Yu
