```mermaid
stateDiagram-v2
[*] --> InitFSM: OnRequestStuff
InitFSM
InitFSM --> StuffFailed: OnError
InitFSM --> StuffSentOut: OnStuffSentOut
StuffFailed
StuffSentOut
StuffSentOut --> StuffFailed: OnError
StuffSentOut --> StuffSuccess: OnStuffSuccess
StuffSuccess
```