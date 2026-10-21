# Asset state transitions

The [generated Bitcoin diagram](fsm.md) describes the existing deposit FSM.
The asset journal currently mirrors deposit state using a separate graph,
shown below. Assets are excluded from ordinary deposit queries and do not
instantiate `deposit.FSM`. The [architecture audit](../../docs/asset-loop-in-architecture.md)
calls for replacing this mirror with optional asset data and asset-aware expiry
actions in the actual deposit FSM.

The asset variant keeps the existing reservation and terminal state names.
It adds an explicit exposure barrier before signatures can leave the client.
Generic failure, restart, or expiry after that barrier keeps the deposits
reserved. Only a confirmed asset claim or refund can finalize them; no event
routes to the Bitcoin-only expiry, withdrawal, or channel-opening actions.

```mermaid
stateDiagram-v2
[*] --> Deposited: OnStart
Deposited --> LoopingIn: OnLoopInInitiated
LoopingIn --> Deposited: OnError before exposure
LoopingIn --> AssetClaimExposed: OnClaimExposed
AssetClaimExposed --> AssetClaimExposed: OnError / OnExpiry / OnRecover
AssetClaimExposed --> LoopedIn: OnLoopedIn with confirmed claim
AssetClaimExposed --> SweepHtlcTimeout: OnSweepingHtlcTimeout
SweepHtlcTimeout --> SweepHtlcTimeout: OnError / OnExpiry / OnRecover
SweepHtlcTimeout --> LoopedIn: OnLoopedIn if claim wins
SweepHtlcTimeout --> HtlcTimeoutSwept: OnHtlcTimeoutSwept with proof
```

This shared graph does not register asset deposits in the production inventory
or replace the shared manager's exclusion with Bitcoin swaps and withdrawals.
The asset coordinator must still call that manager through its reservation
adapter before exposing signatures.
