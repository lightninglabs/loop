# Architecture

`tap-sdk` is a client-side SDK for applications that talk to `tapd`, the
Taproot Assets daemon. The root package exposes the business API; transport
packages own wire formats and connection setup.

The current Go implementation is the source of truth for the public API model,
but the design is intentionally language-portable. Future TypeScript, Rust,
Python, Kotlin, and Swift SDKs should expose the same business concepts even if
their package layout differs.

## System View

```
Application
  │
  │ uses AssetRef, Wallet, Issuer, Universe, TxBuilder
  ▼
tap-sdk root package
  │
  │ Client interface
  ▼
grpc/ or rest/
  │
  │ tapd RPC over TLS + macaroons
  ▼
tapd
  │
  ├─ lnd wallet/signing backend
  └─ Taproot Assets state, proofs, universe, mint batches
```

## Package Boundaries

| Package | Boundary |
|---------|----------|
| `tapsdk` | Business API, domain types, high-level wallet/issuer/universe surfaces, builders, errors |
| `grpc` | Native gRPC client, TLS setup, macaroon auth, proto marshal/unmarshal |
| `rest` | REST client, TLS setup, macaroon auth, JSON marshal/unmarshal, WebSocket event streams |
| `macaroon` | Low-level macaroon source helpers shared by transports |
| `internal/anchor` | Anchor PSBT helpers |
| `internal/codec` | Internal TLV/alt-leaf/STXO encoding helpers |
| `internal/vpsbt` | Virtual PSBT encoding for interactive transfers |

The root package must not import transport packages. Transports import the
root package and return root SDK types. That one-way dependency keeps the
business API stable even if the repo layout changes later.

## Domain Model

The SDK separates user-facing business concepts from tapd protocol details.

| Concept | Meaning |
|---------|---------|
| `AssetRef` | Opaque, storable handle for an asset in the SDK API |
| `Asset` | A fungible token or one NFT item |
| `Collection` | A group of NFT items |
| `Issuance` | One concrete protocol issuance/tranche of a fungible asset |
| `AssetRecord` | Low-level wallet row that still exposes protocol details |
| `Balance` | Confirmed balance for one semantic `AssetRef` |
| `Transfer` / `Burn` | Asset movement or destruction keyed by `AssetRef` |

Grouped fungibles are keyed by group `AssetRef`. Standalone NFTs and NFT
collection items are keyed by asset-ID `AssetRef`. Collections are keyed by
group `AssetRef`, but a collection is not itself an asset.

See [Asset Model](asset-model.md) for more detail.

## High-Level Surfaces

### Wallet

`Wallet` is the primary application surface. It wraps a `Client` and exposes
common operations using `AssetRef`:

- receive addresses
- sends and multi-sends
- balances, assets, issuances, collections, transfers
- proof import/export
- burns
- ownership proofs
- transaction builders
- event listeners
- access to `Issuer` and `Universe`

Low-level RPC-shaped methods remain available through `wallet.Client()` for
advanced callers.

### Issuer

`Issuer` hides tapd mint batch mechanics behind business actions:

- `CreateFungible`
- `IssueFungible`
- `CreateNFT`
- `CreateCollection`
- `MintCollectionItem`

The issuer serializes its own mint operations because tapd has one active mint
batch per daemon. If a mint result times out while resolving, callers should
inspect wallet assets or batches before retrying to avoid duplicate issuance.

### Universe

`Universe` is the high-level discovery and proof surface:

- `HasAsset`
- `GetRoots`
- `ListProofs`
- `GetProof`
- `SyncAsset`
- `SyncAssets`

The high-level API takes `AssetRef`. The low-level `UniverseClient` remains
available for direct protocol-shaped universe operations.

## Transport Model

Both transports satisfy the same root `Client` interface.

```go
type Client interface {
    WalletClient
    WalletKitClient
    ProofClient
    UniverseClient
    MintClient
    EventClient
    Close() error
}
```

The gRPC transport consumes native server-streaming RPCs. The REST transport
uses tapd's grpc-gateway JSON endpoints and WebSocket bridge for event streams.
Both transports map tapd responses to the same root SDK types and error
surface.

## TLS and Macaroons

Transport configs accept typed TLS and macaroon sources:

```go
client, err := tapgrpc.NewClient(&tapgrpc.Config{
    Host:     "localhost:10029",
    Network:  tapsdk.NetworkRegtest,
    TLS:      tapgrpc.TLSFromPath("/path/to/tls.cert"),
    Macaroon: tapsdk.MacaroonFromPath("/path/to/admin.macaroon"),
})
```

TLS supports:

- default tapd certificate path
- certificate file
- certificate PEM data
- system certificate pool
- explicit insecure mode for local tests
- minimum TLS version
- optional SHA-256 certificate pinning

Macaroons can be loaded from one file, a directory of per-service macaroons,
or one hex-encoded macaroon.

See [Transports and Auth](transports.md).

## Events

The low-level `EventClient` streams tapd event records. The high-level
`EventListener` projects those records into SDK business events:

- `MintEvent`
- `SendEvent`
- `ReceiveEvent`

Transfer rows include tapd asset type data. The SDK uses that field to keep
grouped fungibles keyed by group `AssetRef` while NFT collection items stay
keyed by concrete item `AssetRef`.

## Error Surface

SDK methods wrap failures in `*tapsdk.Error` where possible:

```go
type Error struct {
    Op  string
    Err error
}
```

Errors preserve the underlying gRPC status when available, including REST
responses that originate from grpc-gateway. Common conditions also map to
sentinel errors such as `ErrAssetUnknown`, `ErrInsufficientBalance`,
`ErrProofNotFound`, `ErrPermissionDenied`, and `ErrUnsupportedByTapd`.

## Compatibility Boundary

The current SDK targets tapd v0.8.6. Custom-anchor commitment construction
and verification use its spender and split-proof rules. New transition proofs
are always V1; confirmed histories retain their original proof versions.

See [Compatibility](compatibility.md).

## Design Principles

1. **Developer-facing first.** The SDK should expose business operations, not
   raw tapd nouns, unless a caller intentionally drops to the low-level client.
2. **`AssetRef` everywhere.** Application code should store and pass one asset
   handle across wallet, issuer, proof, burn, event, and universe flows.
3. **Honest names.** Assets, collections, and issuances are distinct concepts.
   The SDK should not hide that distinction behind a generic type.
4. **Transport parity.** gRPC and REST should return the same SDK types and
   comparable errors.
5. **Portable API model.** Go package layout should not leak into the long-term
   SDK model intended for other languages.
