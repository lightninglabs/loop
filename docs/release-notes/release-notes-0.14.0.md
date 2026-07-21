# Loop Client Release Notes

- **Release date:** 2021-06-01
- **Release page:**
  [v0.14.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.14.0-beta)
- **Previous release:** [v0.13.0-beta](release-notes-0.13.0.md)
- **Next release:** [v0.14.1-beta](release-notes-0.14.1.md)

#### New Features

- The loopd client reports off-chain routing failures for loop out swaps if it
  cannot find a route to the server for the swap's prepay or invoice payment.
  This allows the server to release accepted invoices, if there are any,
  earlier, reducing the amount of time that funds are held off-chain. If the
  swap failed on one of the loop server's channels, it will report failure
  location of its off-chain failure. If the failure occurred outside of the loop
  server's infrastructure, a generic failure will be used so that no information
  about the client's position in the network is leaked.

#### Breaking Changes

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Carla Kirk-Cohen
- Maurice Poirrier
