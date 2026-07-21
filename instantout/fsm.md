```mermaid
stateDiagram-v2
[*] --> Init: OnStart
BuildHtlc
BuildHtlc --> InstantOutFailed: OnError
BuildHtlc --> PushPreimage: OnHtlcSigReceived
BuildHtlc --> InstantOutFailed: OnRecover
FailedHtlcSweep
FailedHtlcSweep --> PublishHtlcSweep: OnRecover
FinishedHtlcPreimageSweep
FinishedSweeplessSweep
Init
Init --> InstantOutFailed: OnError
Init --> SendPaymentAndPollAccepted: OnInit
Init --> InstantOutFailed: OnRecover
InstantOutFailed
PublishHtlc
PublishHtlc --> FailedHtlcSweep: OnError
PublishHtlc --> PublishHtlcSweep: OnHtlcPublished
PublishHtlc --> PublishHtlc: OnRecover
PublishHtlcSweep
PublishHtlcSweep --> FailedHtlcSweep: OnError
PublishHtlcSweep --> WaitForHtlcSweepConfirmed: OnHtlcSweepPublished
PublishHtlcSweep --> PublishHtlcSweep: OnRecover
PushPreimage
PushPreimage --> InstantOutFailed: OnError
PushPreimage --> PublishHtlc: OnErrorPublishHtlc
PushPreimage --> PushPreimage: OnRecover
PushPreimage --> WaitForSweeplessSweepConfirmed: OnSweeplessSweepPublished
SendPaymentAndPollAccepted
SendPaymentAndPollAccepted --> InstantOutFailed: OnError
SendPaymentAndPollAccepted --> BuildHtlc: OnPaymentAccepted
SendPaymentAndPollAccepted --> InstantOutFailed: OnRecover
WaitForHtlcSweepConfirmed
WaitForHtlcSweepConfirmed --> FailedHtlcSweep: OnError
WaitForHtlcSweepConfirmed --> FinishedHtlcPreimageSweep: OnHtlcSwept
WaitForHtlcSweepConfirmed --> WaitForHtlcSweepConfirmed: OnRecover
WaitForSweeplessSweepConfirmed
WaitForSweeplessSweepConfirmed --> PublishHtlc: OnError
WaitForSweeplessSweepConfirmed --> WaitForSweeplessSweepConfirmed: OnRecover
WaitForSweeplessSweepConfirmed --> FinishedSweeplessSweep: OnSweeplessSweepConfirmed
```