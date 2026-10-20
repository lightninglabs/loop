# Shared asset swap contracts

This Loop module owns the Bitcoin deposit and HTLC policies used by Loop
clients and servers. Its public inputs and outputs are fixed-size byte arrays,
scripts, control blocks, and witness stacks. It has no dependency on tapd, lnd,
wallet implementations, or the Tap SDK runtime.

Loop's existing Bitcoin HTLC and static-address helpers use the same leaf
serializers in this module. Those primitives preserve the original signed
expiry encodings and zero-hash quote templates. Funded asset contracts apply
their stricter key, hash, and timelock checks in the kit constructors.

`NewDepositKit` creates the sorted MuSig2 deposit policy with a client CSV refund.
`NewSwapKit` requires an explicit policy: `SwapHTLCLegacyCSV` retains the original
asset HTLC contract and `SwapHTLCStaticLoopInV3` uses Loop's CLTV refund contract.
Both HTLC policies require a receiver signature and the correct 32-byte preimage
on the success path, with the existing one-block CSV restriction.

The nested Loop module `github.com/lightninglabs/loop/swapkit/tapsdk` adapts
these contracts to SDK `CustomAssetOutput` plans and OP_TRUE asset witnesses.
The generic SDK owns the custom-anchor transaction and proof lifecycle.
Applications using an older Bitcoin wallet graph can import this module directly;
the split btcd v2 script libraries coexist with earlier btcd packages without
upgrading lnd. Such applications still own their proof and packet adapters.

Supply a verified asset commitment root when constructing spend information and
compare the returned Bitcoin script with the authenticated prevout before
signing. The helper does not verify asset provenance, chain maturity, invoices,
or preimage release policy. Asset-level OP_TRUE witnesses must correspond to the
exact asset output being spent.

Run `GOWORK=off go test ./...` in this directory. Tests freeze legacy contract
vectors and execute valid and adversarial Bitcoin witnesses.
