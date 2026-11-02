# Compatibility

The Loop copy shares the module and Go version in Loop's `go.mod` (currently
Go 1.27.3). It uses the pre-btcd-v2 Bitcoin packages and released Taproot Assets
v0.8.6. `btcec/v2` is the existing cryptography package, not the btcd v2 migration.

Custom-anchor flows retain the tapd v0.8.6 proof requirements. Applications
must enforce the supported tapd version before starting these flows. New
commitments request V1 transition proofs, including spender leaves,
split-root STXO proofs, and root-locator proofs. The source backport does not
change activation rules or permit stale or incomplete proof histories.

Compile-time LND dependencies do not set the minimum remote LND version.
Loop's startup gate and tapd's own supported LND versions remain responsible
for their runtime requirements.

Run the preserved library tests with `go test -race ./tapsdk/...`. Daemon-backed
swap flows are tested by the consuming integration suite with proof activation
enabled on regtest. The upstream SDK's standalone Docker harness is not part
of this copy.
