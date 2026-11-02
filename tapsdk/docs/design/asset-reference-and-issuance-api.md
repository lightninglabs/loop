# Asset Reference and Issuance API

## Status

Accepted and implemented for the core Wallet, Issuer, Universe, proof, burn,
event, and balance surfaces.

## Context

Taproot Assets exposes two protocol identifiers that can both look like "the
asset" to application developers:

- `asset_id`: the genesis identifier of one concrete issuance/tranche.
- `group_key`: the identifier that ties multiple issuances into one grouped
  asset family.

The distinction is correct at the protocol layer. It is the wrong default for
an application SDK. Most callers want to store one handle, pass that handle
through every SDK method, and let the SDK decide how to map it to tapd.

The early SDK leaked too much daemon shape:

- separate `asset_id` and `group_key` fields in requests
- raw balance grouping modes
- asset rows that conflated a user-facing asset with an issuance
- collection groups represented as assets
- mint methods that used one verb for both creating a new asset and issuing
  more supply

## Decision

The public SDK asset handle is `tapsdk.AssetRef`.

`AssetRef` is opaque, checksummed, stable for storage, and accepted anywhere a
high-level method refers to an asset or collection.

The SDK also names the business concepts explicitly:

| Type | Meaning |
|------|---------|
| `Asset` | A fungible token or one NFT item |
| `Collection` | A group of NFT items |
| `Issuance` | One concrete fungible issuance/tranche |
| `AssetRecord` | Low-level wallet row with protocol details |

Collections are not assets. Issuances are not assets. This distinction matters
for fungible supply, NFT collection item transfers, burns, proofs, and event
projection.

## AssetRef Encoding

`AssetRef` uses a bech32m string with HRP `tapr`.

Payload:

- 1 byte format version
- 1 byte discriminator
- raw identifier bytes

Discriminators:

- `0x00`: protocol `asset_id`
- `0x01`: protocol `group_key`

The string is intentionally opaque. A human-readable `asset:<hex>` or
`group:<hex>` format would teach callers to care about a distinction the SDK is
trying to abstract.

## Mapping Rules

| Business object | Public ref |
|-----------------|------------|
| Grouped fungible asset | group-key `AssetRef` |
| Ungrouped fungible asset | asset-ID `AssetRef` |
| Standalone NFT | asset-ID `AssetRef` |
| NFT collection | group-key `AssetRef` |
| NFT collection item | item asset-ID `AssetRef` |
| Fungible issuance/tranche | `IssuanceID` plus parent `AssetRef` |

## Public API Rules

Use asset-level names when the caller is working with the user-visible asset:

- `AssetRef`
- `Asset`
- `ListAssets`
- `ListBalances`
- `FetchAssetMeta`
- `CreateFungible`
- `CreateNFT`

Use issuance-level names when the caller or result is about one concrete
protocol issuance:

- `IssuanceID`
- `Issuance`
- `ListIssuances`
- `IssueFungible`

Use collection-level names for NFT collections:

- `Collection`
- `ListCollections`
- `ListCollectionItems`
- `MintCollectionItem`

Avoid exposing raw `Group`, `GroupedAsset`, or `AssetID` naming in high-level
SDK APIs unless the field really is protocol-level diagnostic data.

## Wallet Surface

Wallet methods accept `AssetRef` for the high-level flows:

- receive address creation
- send and multi-send
- balances
- asset, collection, and issuance listing
- transfer history
- proof import/export
- burns
- ownership proofs

The low-level client remains available through `Wallet.Client()` for advanced
callers that intentionally need raw protocol records or tapd-shaped requests.

## Issuer Surface

Minting is split by business action:

| Method | Meaning | Returns |
|--------|---------|---------|
| `CreateFungible` | create a new grouped fungible asset | `*Asset` |
| `IssueFungible` | issue more supply of an existing fungible asset | `*Issuance` |
| `CreateNFT` | create a standalone NFT | `*Asset` |
| `CreateCollection` | create a collection by minting the first item | `*CollectionMintResult` |
| `MintCollectionItem` | mint another NFT into a collection | `*Asset` |

The low-level mint client may still expose tapd batch mechanics for advanced
callers. The high-level issuer should hide batch state by default.

## Universe and Proofs

Universe methods use `AssetRef`:

- `HasAsset`
- `GetRoots`
- `ListProofs`
- `GetProof`
- `SyncAsset`
- `SyncAssets`

Proof export can return multiple entries for a group ref because a grouped
fungible may have multiple issuances. Proof import returns the registered
assets implied by the proof file.

Ownership proof methods are also `AssetRef`-first. The Wallet layer resolves
the concrete issuance ID, script key, and outpoint needed by tapd.

## Consequences

- Normal callers can ignore group key versus asset ID mechanics.
- Fungible assets aggregate across issuances.
- NFT collection item transfers and burns remain keyed by concrete item refs.
- The SDK can expose simpler high-level types while preserving low-level
  records for diagnostics.
- tapd compatibility matters: daemon responses must include enough asset type
  data for the SDK to project records correctly.
