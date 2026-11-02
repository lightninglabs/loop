# Getting Started

This guide shows the common SDK flow: connect to `tapd`, create assets, receive
and send them, inspect balances, export proofs, and query the universe.

## Prerequisites

- The Go version required by Loop (currently 1.27.3)
- `tapd` v0.8.6 for custom-anchor flows
- TLS certificate and macaroon for the `tapd` node

The SDK library tests run from Loop with `go test ./tapsdk/...`.
Daemon-backed workflows are exercised by the consuming integration suite.

## Install

```bash
go get github.com/lightninglabs/loop/tapsdk@<loop-revision>
```

## Connect

Most applications import the root package and one transport package.

```go
client, err := tapgrpc.NewClient(&tapgrpc.Config{
    Host:     "localhost:10029",
    Network:  tapsdk.NetworkRegtest,
    TLS:      tapgrpc.TLSFromPath("/path/to/tls.cert"),
    Macaroon: tapsdk.MacaroonFromPath("/path/to/admin.macaroon"),
})
if err != nil {
    return err
}
defer client.Close()

wallet := tapsdk.NewWallet(client, tapsdk.NetworkRegtest)
```

REST uses the same root SDK types:

```go
client, err := taprest.NewClient(&taprest.Config{
    BaseURL:  "https://localhost:8089",
    Network:  tapsdk.NetworkRegtest,
    TLS:      taprest.TLSFromPath("/path/to/tls.cert"),
    Macaroon: tapsdk.MacaroonFromPath("/path/to/admin.macaroon"),
})
```

## Issue a Fungible Asset

```go
issuer := wallet.NewIssuer()

token, err := issuer.CreateFungible(ctx, tapsdk.FungibleAssetSpec{
    Name:   "example-token",
    Amount: 1_000_000,
})
if err != nil {
    return err
}

_, err = issuer.IssueFungible(ctx, token.AssetRef, 500_000)
if err != nil {
    return err
}
```

Use `token.AssetRef` everywhere else in the SDK. You do not need to choose
between an asset ID and group key.

## Create NFTs and Collections

```go
nft, err := issuer.CreateNFT(ctx, tapsdk.NFTSpec{
    Name: "ticket-001",
})
if err != nil {
    return err
}

created, err := issuer.CreateCollection(ctx, tapsdk.NFTSpec{
    Name: "series-001",
})
if err != nil {
    return err
}

item, err := issuer.MintCollectionItem(
    ctx, created.Collection.AssetRef, tapsdk.NFTSpec{
        Name: "series-002",
    },
)
```

`nft` and `item` are assets. `created.Collection` is a collection.

## Receive and Send

The high-level receive helper defaults to V2 addresses.

```go
addr, err := wallet.NewReceiveAddress(ctx, token.AssetRef)
if err != nil {
    return err
}
```

Send with an explicit amount:

```go
transfer, err := wallet.Send(ctx, addr.Encoded, tapsdk.WithAmount(100))
if err != nil {
    return err
}

fmt.Println(transfer.AnchorTxid)
```

Send multiple recipients of the same asset:

```go
batch, err := wallet.SendMulti(ctx, []tapsdk.Recipient{
    tapsdk.RecipientWithAmount(firstAddr, 100),
    tapsdk.RecipientWithEmbeddedAmount(secondAddr),
})
```

## Balances and Assets

```go
balance, err := wallet.GetBalance(ctx, token.AssetRef)
if err != nil {
    return err
}

assets, err := wallet.ListAssets(ctx, &tapsdk.ListAssetsRequest{})
if err != nil {
    return err
}

collections, err := wallet.ListCollections(
    ctx, &tapsdk.ListCollectionsRequest{},
)
```

`ListAssets` returns fungible assets and concrete NFT item assets.
`ListCollections` returns collection objects.

## Proofs

Export one or more proof entries for an `AssetRef`:

```go
bundle, err := wallet.ExportProof(ctx, token.AssetRef)
if err != nil {
    return err
}
```

Import a proof bundle received out of band:

```go
registered, err := wallet.ImportProof(ctx, bundle)
if err != nil {
    return err
}

fmt.Println(len(registered))
```

When a group `AssetRef` maps to multiple issuances, proof export returns the
set of proof entries needed for that asset.

## Burns

```go
burn, err := wallet.Burn(ctx, token.AssetRef, 10,
    tapsdk.WithBurnNote("redeemed"),
)
if err != nil {
    return err
}

fmt.Println(burn.Amount)
```

## Universe

```go
universe := wallet.NewUniverse()

known, err := universe.HasAsset(ctx, token.AssetRef)
if err != nil {
    return err
}

proofs, err := universe.ListProofs(ctx, token.AssetRef)
if err != nil {
    return err
}
```

Universe sync hosts should come from trusted configuration:

```go
_, err = universe.SyncAsset(ctx, token.AssetRef, "tapd.example:10029")
```

Current tapd versions perform the remote universe dial. Do not pass direct user
input as the sync host.

## Next Steps

- Read the [Asset Model](asset-model.md).
- Configure production [Transports and Auth](transports.md).
- Check the local [Compatibility](compatibility.md) requirements.
