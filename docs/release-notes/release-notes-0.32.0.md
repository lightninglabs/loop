# Loop Client Release Notes

- **Release date:** 2026-03-02
- **Release page:**
  [v0.32.0-beta](https://github.com/lightninglabs/loop/releases/tag/v0.32.0-beta)
- **Previous release:** [v0.31.8-beta](release-notes-0.31.8.md)
- **Next release:** [v0.32.1-beta](release-notes-0.32.1.md)

#### New Features

Static Address Open Channel — This release introduces the ability to open
Lightning channels directly from static address deposits. Instead of performing
a loop-in to get funds into your Lightning node and then opening a channel as
separate steps, you can now combine both into a single operation using the new
loop openchannel command.

  - Consolidate deposits into channels: If you have one or more static address
    deposits sitting on-chain, you can open a channel to any peer using those
    funds directly, skipping the loop-in swap and its associated fees.
  - Flexible funding: Use the --utxo flag to select specific deposit outpoints,
    or use --fundmax to sweep all selected UTXOs into a single channel.
  - Full channel configuration: The command supports the same options as lncli
    openchannel — channel type (tweakless, anchors, taproot), private channels,
    push amounts, fee rates, zero-conf, and more.

Examples:

```shell
loop openchannel --node_key <peer_pubkey> --local_amt 1000000
loop openchannel --node_key <peer_pubkey> --utxo txid:0 --fundmax
```

#### Breaking Changes

#### Bug Fixes

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Slyghtning
