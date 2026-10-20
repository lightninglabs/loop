The Bitcoin diagram below is unchanged. `deposit/flow` owns the transition
names and graph; the legacy deposit FSM binds its wallet actions to that graph.
The asset execution journal uses the `ServerAsset` variant, shown afterward.
Ownership, atomic deposit reservations, and verified chain evidence remain
responsibilities of the caller and shared deposit manager.

```mermaid
stateDiagram-v2
[*] --> Deposited: OnStart
ChannelPublished
ChannelPublished --> ChannelPublished: OnExpiry
Deposited
Deposited --> Deposited: OnError
Deposited --> PublishExpirySweep: OnExpiry
Deposited --> LoopingIn: OnLoopInInitiated
Deposited --> OpeningChannel: OnOpeningChannel
Deposited --> Deposited: OnRecover
Deposited --> SweepHtlcTimeout: OnSweepingHtlcTimeout
Deposited --> Withdrawing: OnWithdrawInitiated
Expired
Expired --> Expired: OnExpiry
HtlcTimeoutSwept
HtlcTimeoutSwept --> HtlcTimeoutSwept: OnExpiry
LoopedIn
LoopedIn --> LoopedIn: OnExpiry
LoopingIn
LoopingIn --> Deposited: OnError
LoopingIn --> PublishExpirySweep: OnExpiry
LoopingIn --> LoopingIn: OnLoopInInitiated
LoopingIn --> LoopedIn: OnLoopedIn
LoopingIn --> LoopingIn: OnRecover
LoopingIn --> SweepHtlcTimeout: OnSweepingHtlcTimeout
OpeningChannel
OpeningChannel --> ChannelPublished: OnChannelPublished
OpeningChannel --> Deposited: OnError
OpeningChannel --> OpeningChannel: OnExpiry
OpeningChannel --> OpeningChannel: OnRecover
PublishExpirySweep
PublishExpirySweep --> Deposited: OnError
PublishExpirySweep --> WaitForExpirySweep: OnExpiryPublished
PublishExpirySweep --> PublishExpirySweep: OnRecover
SweepHtlcTimeout
SweepHtlcTimeout --> HtlcTimeoutSwept: OnHtlcTimeoutSwept
SweepHtlcTimeout --> SweepHtlcTimeout: OnRecover
WaitForExpirySweep
WaitForExpirySweep --> Deposited: OnError
WaitForExpirySweep --> Expired: OnExpirySwept
WaitForExpirySweep --> PublishExpirySweep: OnRecover
Withdrawing
Withdrawing --> Deposited: OnError
Withdrawing --> Withdrawing: OnExpiry
Withdrawing --> Withdrawing: OnRecover
Withdrawing --> Withdrawing: OnWithdrawInitiated
Withdrawing --> Withdrawn: OnWithdrawn
Withdrawn
Withdrawn --> Withdrawn: OnExpiry
Withdrawn --> Withdrawn: OnWithdrawn
```

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
