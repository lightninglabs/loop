# Loop Taproot Assets SDK

This package is Loop's maintained copy of the Taproot Assets SDK. Import
`github.com/lightninglabs/loop/tapsdk`, with transports in `tapsdk/grpc` and
`tapsdk/rest`. It shares Loop's module and dependency versions.

The source was imported from
[hieblmi/tap-sdk at f03d1cbf5f1b](https://github.com/hieblmi/tap-sdk/tree/f03d1cbf5f1bbb4103ebdfc4bcf0cca377948a11),
based on upstream tap-sdk v0.3.0. The original MIT license is retained in
[LICENSE](LICENSE). This includes the custom-anchor transfer engine, passive
assets, signing packages, proof confirmation and registration, exact outpoint
proof lookups, received-input signing metadata, and the borrowed gRPC
connection constructor.

Bitcoin types are backported to the pre-btcd-v2 packages used by Loop. This
is a source compatibility change; it does not downgrade Taproot Assets proof
rules or make custom-anchor flows safe against older tapd versions. The native
engine uses released Taproot Assets v0.8.6. Applications must still enforce
their supported daemon version and validate current chain/proof state before
signing or accepting funds.

The SDK contains wallet and transport operations. Swap contracts, approval
policies, chain checks and recovery belong in the separate `swapkit` packages.
The application owns its RPC connections, signing authority and durable swap
state. See [transports](docs/transports.md) for connection ownership and auth.

Upstream library code and unit tests are retained in this directory. To test:

```sh
go test -race ./tapsdk/...
```

Upstream demos, release tooling and the standalone Docker integration harness
are not copied. Consumers exercise daemon-backed flows in their integration
suites. When importing upstream fixes, preserve the local import paths and
legacy Bitcoin dependency graph, and run the library and consumer tests.

Consuming binaries must copy the `github.com/lightningnetwork/lnd/tlv`
replacement from Loop's `go.mod`. It selects the bounded legacy decoder shipped
with tapd v0.8.6; Go does not inherit dependency replacements. Plain tlv v1.3.2
can allocate from malformed record lengths before returning a decode error.
