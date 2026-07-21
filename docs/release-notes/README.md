# Loop Client Release Notes

Release notes are listed in chronological order and include every published
GitHub release. Previous and next links connect adjacent files in this index.
Contributor lists are derived from non-merge commit authors and co-authors
between consecutive official release tags.

Add user-facing changes to [release-notes-next.md](release-notes-next.md).
When preparing a release, use
[release-notes-template.md](release-notes-template.md) to turn those notes into
a versioned file, then reset the next-release sections.

| Release notes | Date | Highlights |
| --- | --- | --- |
| [v0.1-alpha](release-notes-0.1.md) | 2019-03-21 | Initial Loop Out release |
| [v0.1.1-alpha](release-notes-0.1.1.md) | 2019-04-17 | Added testnet Loop In |
| [v0.1.2-alpha](release-notes-0.1.2.md) | 2019-05-03 | Fixed macaroon, testnet, and routing fee handling |
| [v0.1.3-alpha](release-notes-0.1.3.md) | 2019-05-31 | Added Docker support and accounting improvements |
| [v0.1.3-beta](release-notes-0.1.3-beta.md) | 2019-05-31 | Published the v0.1.3 beta tag with Docker and accounting updates |
| [v0.2-alpha](release-notes-0.2.md) | 2019-06-26 | Introduced Loop In |
| [v0.2.0-alpha](release-notes-0.2.0.md) | 2019-06-26 | Aliased v0.2-alpha without source changes |
| [v0.2.1-alpha](release-notes-0.2.1.md) | 2019-07-25 | Added configurable Loop Out sweep confirmation targets |
| [v0.2.2-alpha](release-notes-0.2.2.md) | 2019-07-31 | Improved external Loop Out validation |
| [v0.2.3-alpha](release-notes-0.2.3.md) | 2019-10-03 | Fixed REST parity, fee estimation, and confirmation targets |
| [v0.2.4-alpha](release-notes-0.2.4.md) | 2019-10-11 | Prepared flexible swap fees and timing |
| [v0.3.0-alpha](release-notes-0.3.0.md) | 2019-11-21 | Added delayed, batchable Loop Out execution |
| [v0.3.1-alpha](release-notes-0.3.1.md) | 2020-01-15 | Added fee controls for Loop Out and fast quotes |
| [v0.4.0-rc1.beta](release-notes-0.4.0-rc1.md) | 2020-01-24 | Previewed authenticated requests and fixed Loop In timeouts |
| [v0.4.0-beta](release-notes-0.4.0.md) | 2020-02-04 | Added authenticated requests and REST monitoring |
| [v0.4.1-beta](release-notes-0.4.1.md) | 2020-02-11 | Added REST CORS controls and fixed delayed quote errors |
| [v0.5.0-beta](release-notes-0.5.0.md) | 2020-03-05 | Added Loop In last-hop selection |
| [v0.5.1-beta](release-notes-0.5.1.md) | 2020-03-15 | Improved Loop In quotes and fee sanity checks |
| [v0.6.0-beta](release-notes-0.6.0.md) | 2020-04-30 | Added LND v0.10 support and Loop In confirmation targets |
| [v0.6.1-beta](release-notes-0.6.1.md) | 2020-05-12 | Added native SegWit Loop In HTLCs and fixed multipart limits |
| [v0.6.2-beta](release-notes-0.6.2.md) | 2020-05-12 | Bumped the release version after v0.6.1 |
| [v0.6.3-beta](release-notes-0.6.3.md) | 2020-06-04 | Restored multi-channel Loop Out with LND v0.10.1 |
| [v0.6.4-beta](release-notes-0.6.4.md) | 2020-06-12 | Improved Loop Out restart behavior and liquidity speed |
| [v0.6.5-beta](release-notes-0.6.5.md) | 2020-07-02 | Added human-readable server messages |
| [v0.7.0-beta](release-notes-0.7.0.md) | 2020-07-21 | Added longer confirmation targets and server status subscriptions |
| [v0.8.0-beta](release-notes-0.8.0.md) | 2020-08-11 | Improved swap failure reporting and confirmation controls |
| [v0.8.1-beta](release-notes-0.8.1.md) | 2020-08-28 | Reduced Loop Out routing and sweep fees |
| [v0.9.0-beta](release-notes-0.9.0.md) | 2020-09-10 | Introduced liquidity management and TLS |
| [v0.10.0-beta](release-notes-0.10.0.md) | 2020-10-13 | Added multipath Loop In and fee-aware swap suggestions |
| [v0.11.0-beta](release-notes-0.11.0.md) | 2020-10-27 | Introduced Autoloop and transaction labels |
| [v0.11.1-beta](release-notes-0.11.1.md) | 2020-11-12 | Added swap initiator metadata |
| [v0.11.2-beta](release-notes-0.11.2.md) | 2020-12-08 | Added configurable Autoloop swap size limits |
| [v0.11.3-beta](release-notes-0.11.3.md) | 2021-02-09 | Added locked-LND startup waiting and single macaroon support |
| [v0.11.4-beta](release-notes-0.11.4.md) | 2021-02-11 | Defaulted to LND's admin macaroon path |
| [v0.12.0-beta](release-notes-0.12.0.md) | 2021-03-06 | Added per-peer Autoloop rules |
| [v0.12.1-beta](release-notes-0.12.1.md) | 2021-03-26 | Added verbose quote and swap output |
| [v0.12.2-beta](release-notes-0.12.2.md) | 2021-04-29 | Retried failed LSAT payments |
| [v0.13.0-beta](release-notes-0.13.0.md) | 2021-05-19 | Raised the minimum LND version to v0.11.1-beta |
| [v0.14.0-beta](release-notes-0.14.0.md) | 2021-06-01 | Reported Loop Out routing failures to the server |
| [v0.14.1-beta](release-notes-0.14.1.md) | 2021-06-09 | Removed protobuf startup warnings |
| [v0.14.2-beta](release-notes-0.14.2.md) | 2021-07-20 | Fixed Python gRPC client connectivity |
| [v0.15.0-beta](release-notes-0.15.0.md) | 2021-08-03 | Added Loop In quote probing |
| [v0.15.1-beta](release-notes-0.15.1.md) | 2021-11-18 | Updated to LND v0.14 and added WASM RPC stubs |
| [v0.16.0-beta](release-notes-0.16.0.md) | 2021-12-14 | Added private Loop In route hints |
| [v0.17.0-beta](release-notes-0.17.0.md) | 2022-02-07 | Added Loop In support to Autoloop |
| [v0.18.0-beta](release-notes-0.18.0.md) | 2022-03-30 | Added optional routing plugins |
| [v0.19.1-beta](release-notes-0.19.1.md) | 2022-06-09 | Persisted liquidity parameters |
| [v0.20.0-beta](release-notes-0.20.0.md) | 2022-07-20 | Added experimental P2TR HTLCs and MuSig2 sweeps |
| [v0.20.1-beta](release-notes-0.20.1.md) | 2022-08-01 | Improved sweep transaction logging |
| [v0.20.2-beta](release-notes-0.20.2.md) | 2022-10-26 | Raised the minimum LND version for Taproot support |
| [v0.21.0-beta](release-notes-0.21.0.md) | 2022-12-16 | Added Autoloop destination addresses |
| [v0.22.0-beta](release-notes-0.22.0.md) | 2023-03-29 | Added recurring Autoloop budgets |
| [v0.22.1-beta](release-notes-0.22.1.md) | 2023-04-24 | Corrected the HTLC script version for legacy swaps |
| [v0.23.0-beta](release-notes-0.23.0.md) | 2023-04-25 | Made MuSig2 the default swap protocol |
| [v0.24.0-beta](release-notes-0.24.0.md) | 2023-05-24 | Added Easy Autoloop |
| [v0.24.1-beta](release-notes-0.24.1.md) | 2023-05-26 | Added REST bindings for GetInfo |
| [v0.25.0-beta](release-notes-0.25.0.md) | 2023-07-03 | Added SQL stores and Easy Autoloop fee fixes |
| [v0.25.1-beta](release-notes-0.25.1.md) | 2023-07-04 | Removed Windows 386 release builds |
| [v0.25.2-beta](release-notes-0.25.2.md) | 2023-07-04 | Removed unsupported release platforms |
| [v0.26.0-beta](release-notes-0.26.0.md) | 2023-07-28 | Added xpub-backed Loop Out destinations |
| [v0.26.1-beta](release-notes-0.26.1.md) | 2023-08-08 | Recorded swap initiators and repaired faulty timestamps |
| [v0.26.2-beta](release-notes-0.26.2.md) | 2023-08-09 | Migrated faulty Loop Out timestamps |
| [v0.26.3-beta](release-notes-0.26.3.md) | 2023-09-18 | Added the FSM foundation and hardened Loop In settlement |
| [v0.26.4-beta](release-notes-0.26.4.md) | 2023-10-04 | Added Easy Autoloop destinations and Apple Silicon builds |
| [v0.26.5-beta](release-notes-0.26.5.md) | 2023-10-31 | Fixed leap-year parsing and refactored database startup |
| [v0.26.6-beta](release-notes-0.26.6.md) | 2023-11-28 | Added Loop In abandonment and specific balance failures |
| [v0.27.0-beta](release-notes-0.27.0.md) | 2024-01-30 | Added batched Loop Out sweeps |
| [v0.27.1-beta](release-notes-0.27.1.md) | 2024-02-14 | Automatically recovered incorrectly funded external Loop Ins |
| [v0.28.0-beta](release-notes-0.28.0.md) | 2024-03-05 | Expanded Instant Out listing, destinations, and fee accounting |
| [v0.28.1-beta](release-notes-0.28.1.md) | 2024-04-16 | Routed Loop Out prepayments over selected channels |
| [v0.28.2-beta](release-notes-0.28.2.md) | 2024-05-25 | Renamed LSAT options and APIs to L402 |
| [v0.28.3-beta](release-notes-0.28.3.md) | 2024-06-03 | Corrected stored Loop Out costs and batch restoration |
| [v0.28.4-beta](release-notes-0.28.4.md) | 2024-06-04 | Fixed pending-swap cost migration and added RPC timeouts |
| [v0.28.5-beta](release-notes-0.28.5.md) | 2024-06-06 | Paginated cost-migration payment lookups |
| [v0.28.6-beta](release-notes-0.28.6.md) | 2024-07-11 | Expanded sweep-batcher signing and fee controls |
| [v0.28.7-beta](release-notes-0.28.7.md) | 2024-08-08 | Improved sweep selection and stored swap metadata |
| [v0.28.8-beta](release-notes-0.28.8.md) | 2024-10-21 | Introduced the notification manager and mixed sweep batches |
| [v0.28.9-beta](release-notes-0.28.9.md) | 2024-10-30 | Added L402 retrieval and FSM context propagation |
| [v0.29.0-beta](release-notes-0.29.0.md) | 2024-12-18 | Persisted static address Loop In mode |
| [v0.29.1-beta](release-notes-0.29.1.md) | 2025-03-18 | Added Taproot Asset Loop Outs and partial static withdrawals |
| [v0.30.0-beta](release-notes-0.30.0.md) | 2025-03-27 | Added Taproot Asset Easy Autoloop and hardened recovery |
| [v0.31.0-beta](release-notes-0.31.0.md) | 2025-04-25 | Added filtering and pagination to loop listswaps |
| [v0.31.1-beta](release-notes-0.31.1.md) | 2025-05-05 | Changed Autoloop to use a slow publication deadline |
| [v0.31.2-beta](release-notes-0.31.2.md) | 2025-06-18 | Corrected commit hash and version reporting |
| [v0.31.3-beta](release-notes-0.31.3.md) | 2025-09-25 | Added partial static Loop Ins and hardened static address recovery |
| [v0.31.4-beta](release-notes-0.31.4.md) | 2025-10-16 | Added fast static Loop Ins and reproducible releases |
| [v0.31.5-beta](release-notes-0.31.5.md) | 2025-10-31 | Added swap resumption and notification versioning |
| [v0.31.5-beta-lnd0.20](release-notes-0.31.5-lnd0.20.md) | 2025-10-31 | Updated LND 0.20 compatibility and module version metadata |
| [v0.31.6-beta](release-notes-0.31.6.md) | 2025-11-26 | Added Easy Autoloop peer exclusions and loop stop |
| [v0.31.7-beta](release-notes-0.31.7.md) | 2025-11-26 | Bumped the release version after v0.31.6 |
| [v0.31.8-beta](release-notes-0.31.8.md) | 2026-02-03 | Added manual HTLC sweeps and PSBT static withdrawals |
| [v0.32.0-beta](release-notes-0.32.0.md) | 2026-03-02 | Opened channels directly from static address deposits |
| [v0.32.1-beta](release-notes-0.32.1.md) | 2026-03-02 | Updated reproducible releases to Go 1.26 |
| [v0.33.0-beta](release-notes-0.33.0.md) | 2026-04-08 | Added static Loop In fee caps and lifecycle hardening |
| [v0.33.1-beta](release-notes-0.33.1.md) | 2026-05-26 | Added Static Address Autoloop and CLI session tests |
| [v0.33.2-beta](release-notes-0.33.2.md) | 2026-06-08 | Exposed static swap details and hardened MuSig2 inputs |
| [v0.33.3-beta](release-notes-0.33.3.md) | 2026-06-21 | Raised the LND floor and updated the LND dependency |
| [v0.34.0-beta](release-notes-0.34.0.md) | 2026-07-23 | Tracked low-confirmation deposits and production Taproot channels |
| [Next release](release-notes-next.md) | Unreleased | No changes yet |
