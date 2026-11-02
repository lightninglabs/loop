# Asset Model

Taproot Assets has protocol-level identifiers that are correct for the daemon
but awkward for application developers. `tap-sdk` exposes a business model on
top of those identifiers.

## Core Types

| Type | Meaning |
|------|---------|
| `AssetRef` | Opaque SDK handle for one asset or collection |
| `Asset` | A fungible token or one NFT item |
| `Collection` | A group of NFT items |
| `Issuance` | One concrete fungible issuance/tranche |
| `AssetRecord` | Low-level wallet row with protocol details |

## AssetRef

`AssetRef` is the value application code should store, pass around, and expose
over APIs. It is a checksummed bech32m string that internally encodes one tapd
identifier:

- group key for grouped fungible assets
- asset ID for standalone NFTs and NFT collection items
- group key for NFT collections
- asset ID for ungrouped fungibles

The caller does not need to choose between `asset_id` and `group_key`. The SDK
does that at the transport boundary.

## Fungible Assets

A fungible asset can have one issuance or many issuances. Users normally care
about the aggregate asset, not about individual tranches.

```go
token, err := issuer.CreateFungible(ctx, tapsdk.FungibleAssetSpec{
    Name:   "example-token",
    Amount: 1_000_000,
})

more, err := issuer.IssueFungible(ctx, token.AssetRef, 500_000)
```

`CreateFungible` returns an `Asset`. `IssueFungible` returns an `Issuance`.
That distinction is intentional: the asset is the thing users hold; the
issuance is one protocol event that increased supply.

## NFTs and Collections

A standalone NFT is an `Asset` keyed by asset ID.

```go
nft, err := issuer.CreateNFT(ctx, tapsdk.NFTSpec{
    Name: "ticket-001",
})
```

An NFT collection is not an asset. It is a grouping of NFT item assets.

```go
created, err := issuer.CreateCollection(ctx, tapsdk.NFTSpec{
    Name: "collection-001",
})

item, err := issuer.MintCollectionItem(
    ctx, created.Collection.AssetRef, tapsdk.NFTSpec{
        Name: "collection-002",
    },
)
```

Use `Collection.AssetRef` when asking to receive any item from a collection.
Once a concrete item has been minted, transferred, burned, or proven, SDK
records use the item's own `AssetRef`.

## Lists and Balances

High-level wallet list methods preserve the business model:

| Method | Returns |
|--------|---------|
| `Wallet.ListAssets` | semantic assets: fungibles and NFT items |
| `Wallet.ListCollections` | NFT collections |
| `Wallet.ListCollectionItems` | concrete NFT items in a collection |
| `Wallet.ListIssuances` | concrete fungible issuances/tranches |
| `Wallet.ListBalances` | balances keyed by `AssetRef` |
| `Wallet.GetBalance` | aggregate balance for one `AssetRef` |

Low-level records remain available through `Wallet.Client()` when a caller
needs protocol inspection details.

## Proofs, Burns, Events, and Universe

The same `AssetRef` handle is used across the rest of the high-level surface:

- `Wallet.ExportProof`
- `Wallet.ImportProof`
- `Wallet.Burn`
- `Wallet.ListBurns`
- `Wallet.ListTransfers`
- `Wallet.ProveOwnership`
- `Wallet.NewUniverse().HasAsset`
- `Wallet.NewUniverse().ListProofs`
- `EventListener` handlers

When a flow needs a concrete issuance internally, the SDK resolves it before
calling tapd.
