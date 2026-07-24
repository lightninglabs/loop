# Loop Client Release Notes

- **Release date:** 2021-07-20
- **Release page:**
  [v0.14.2-beta](https://github.com/lightninglabs/loop/releases/tag/v0.14.2-beta)
- **Previous release:** [v0.14.1-beta](release-notes-0.14.1.md)
- **Next release:** [v0.15.0-beta](release-notes-0.15.0.md)

#### New Features

#### Breaking Changes

#### Bug Fixes

- Certain versions of the Python gRPC library
  [weren't able to connect](https://github.com/grpc/grpc/issues/23172) to
  `loopd` 's gRPC interface, getting the `missing selected ALPN property` error.
  A server side fix was introduced to get rid of that error message.

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Carla Kirk-Cohen
- Olaoluwa Osuntokun
- Oliver Gugger
