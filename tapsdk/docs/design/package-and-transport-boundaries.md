# Package and Transport Boundaries

## Status

Accepted.

## Context

Go package paths are part of the public API. Moving a public type from one
subpackage to another is a breaking change, even if the type name does not
change.

The SDK needs a clean public API now, but it also needs room to change its
repository layout later as TypeScript, Rust, Python, Kotlin, and Swift SDKs are
added.

## Decision

The root Go package owns SDK business types and high-level surfaces:

- `AssetRef`
- `Asset`
- `Collection`
- `Issuance`
- `Wallet`
- `Issuer`
- `Universe`
- transfer, proof, burn, event, ownership, and balance types

The `grpc` and `rest` packages remain public transport packages. They own
connection setup, TLS config, macaroon auth, and tapd marshal/unmarshal logic.

Internal helpers such as vPSBT encoding, anchor PSBT helpers, and STXO/alt-leaf
code live under `internal/`.

## Consequences

Normal application code imports:

```go
import (
    tapsdk "github.com/lightninglabs/loop/tapsdk"
    tapgrpc "github.com/lightninglabs/loop/tapsdk/grpc"
)
```

The root package does not expose transport constructors. Doing so would force
the root package to import `grpc` and `rest`, while those transport packages
already import the root package to return SDK types. That creates a Go import
cycle.

Keeping transports explicit is the smaller cost:

- business types stay discoverable from one root import
- transport setup stays honest and explicit
- the root API can remain stable if internal folders move
- future language SDKs can mirror the same model without inheriting Go's
  package constraints

## Non-Goals

- Hiding that a Go caller must choose gRPC or REST.
- Making `grpc` and `rest` private.
- Exposing internal vPSBT, anchor, or codec helpers as extension points before
  there is a concrete advanced-use API.
