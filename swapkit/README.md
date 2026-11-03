# Shared asset swap contracts

This package uses the Bitcoin contracts and flow records from Kon's static
asset Loop In implementation as the common contract layer for both asset swap
directions. The old asset deposit, HTLC and sweep engines are replaced by these
contracts and the native SDK adapter in [`tapsdk`](tapsdk/README.md).

The root package is pure Bitcoin policy: fixed-size public keys, leaves,
control blocks, spend constraints and witness stacks. It has no RPC clients,
wallet implementation or Tap SDK dependency. Weight helpers use lnd's
transaction estimator types. Ordinary Bitcoin HTLC,
static-address and reservation helpers call the same leaf serializers here.
The primitive serializers preserve signed expiry encodings and zero-hash quote
templates; funded constructors validate their keys, hashes and timelocks.

| Constructor | Cooperative path | Unilateral refund |
| --- | --- | --- |
| `NewDepositKit` | Sorted MuSig2 | Sender CSV delay |
| `NewReservationKit` | Sorted MuSig2 | Server/sender CLTV height |
| `NewSwapKit(SwapHTLCLegacyCSV)` | Sorted MuSig2 | Sender CSV delay |
| `NewSwapKit(SwapHTLCStaticLoopInV3)` | Sorted MuSig2 | Sender CLTV height |

Both HTLC policies share the receiver signature and correct 32-byte preimage
success path, including its existing one-block CSV. `SwapHTLCCLTV` is a
same-value, direction-neutral alias for `SwapHTLCStaticLoopInV3`. Policy values
and legacy contract vectors remain stable.

Sender and receiver follow the direction of the on-chain assets. For Loop In,
the client sends and refunds, and the server claims with the preimage. For Loop
Out, the server sends and refunds, and the client claims. Internal keys control
the cooperative MuSig2 path; script keys control unilateral paths. A party may
reuse its own key across those roles, while cross-party x-only aliases are
rejected.

`SwapKitParams` retains Kon's `SenderKey`, `ReceiverKey`, internal-key fields,
`PaymentHash` and policy-selected `Expiry`. `SenderScriptKey` and
`ReceiverScriptKey` are explicit script-role aliases; conflicting aliases are
rejected. `DepositKitParams` retains the original `ClientKey`/`ServerKey` form,
or accepts all four explicit roles. `ReservationKitParams` similarly accepts
`ServerKey`/`ClientKey`, or four roles, with a distinct `RefundHeight`. Mixing
the two-key convenience form and explicit roles is rejected.

Supply a verified complete asset commitment root to construct spend
information, then compare its Bitcoin script with the authenticated prevout.
Apply the returned sequence and locktime before signing any inputs, including
fee inputs. CSV uses a relative block delay; CLTV uses a non-final sequence with
BIP68 disabled and an absolute height. New CLTV refunds use `0xfffffffd`
to signal RBF; already signed transactions keep their original sequence. Chain maturity, confirmation depth,
Lightning deadlines, invoice state and authority release remain owned by the
calling FSM.

The [`tapsdk`](tapsdk/README.md) adapter owns conversion to native SDK requests,
proof validation and package handling. The SDK owns generic transfer
construction, allocation, virtual transitions, proof processing and sealed
recovery packages. LND and tapd retain their normal signing and publication
roles. The asset-level OP_TRUE descriptor is independent of the Bitcoin
contract; the fixed NUMS construction remains the policy for new contracts in
this integration, and funded outputs retain their original descriptor.

`SwapAnchorSpendInfo.MaxSuccessWitnessSize` and `MaxTimeoutWitnessSize`
include the complete asset control block and a maximum 65-byte signature.
`AddSuccessToEstimator` and `AddTimeoutToEstimator` apply these sizes to lnd's
transaction weight estimator. They describe witnesses emitted by this kit,
without an annex.

Run `go test ./swapkit` from the Loop module. Tests freeze legacy contract
vectors and execute valid and adversarial Bitcoin witnesses, including
independent key roles and server-funded CLTV reservations.
