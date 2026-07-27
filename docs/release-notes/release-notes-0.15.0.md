# Loop Client Release Notes

- **Release date:** 2021-08-03
- **Release page:**
  [v0.15.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.15.0-beta)
- **Previous release:** [v0.14.2-beta](release-notes-0.14.2.md)
- **Next release:** [v0.15.1-beta](release-notes-0.15.1.md)

#### New Features

* Loop-in quote now asks the server to optionally probe the client to test
  inbound liquidity. The server may use this information to give more accurate
  quotes.

#### Breaking Changes

#### Bug Fixes

* Grpc error codes returned by the swap server when swap initiation fails are
  now surfaced to the client. Previously these error codes would be returned as
  a string.

#### Maintenance

* Updated compile time dependencies of `lnd`, `grpc-gateway`, `protobuf` and
  `grpc`.

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Carla Kirk-Cohen
- Oliver Gugger
- Yong Yu
