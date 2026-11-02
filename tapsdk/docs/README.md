# Documentation

This directory contains narrative documentation for `tap-sdk`. The root
[README](../README.md) is intentionally short; use these docs for the actual
developer and architecture details.

## Guides

- [Getting Started](getting-started.md) — connect to tapd and run the common
  wallet/issuer/universe flows.
- [Asset Model](asset-model.md) — `AssetRef`, assets, collections, and
  issuances.
- [Transports and Auth](transports.md) — gRPC, REST, TLS, and macaroon setup.
- [Compatibility](compatibility.md) — supported tapd, lnd, and Go versions.
- [Architecture](architecture.md) — package boundaries and design principles.

## Design Decisions

Durable design records live in [docs/design](design/README.md). They document
why the public API is shaped the way it is, and are useful context for humans
and coding agents before making API changes.
