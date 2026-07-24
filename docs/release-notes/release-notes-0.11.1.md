# Loop Client Release Notes

- **Release date:** 2020-11-12
- **Release page:**
  [v0.11.1-beta](https://github.com/lightninglabs/loop/releases/tag/v0.11.1-beta)
- **Previous release:** [v0.11.0-beta](release-notes-0.11.0.md)
- **Next release:** [v0.11.2-beta](release-notes-0.11.2.md)

#### New Features

* When requesting a swap, a new `initiator` field can be set on the gRPC/REST
  interface that is appended to the user agent string that is sent to the server
  to give information about Loop usage. The initiator field is meant for user
  interfaces to add their name to give the full picture of the binary used
  (`loopd`, LiT) and the method/interface used for triggering the swap (`loop`
  CLI, autolooper, LiT UI, other 3rd party UI).

#### Breaking Changes

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Carla Kirk-Cohen
- Oliver Gugger
