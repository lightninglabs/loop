```mermaid
stateDiagram-v2
[*] --> InitFSM: OnRequestStuff
InitFSM
InitFSM --> StuffSentOut: OnStuffSentOut
InitFSM --> StuffFailed: OnError
StuffFailed
StuffSentOut
StuffSentOut --> StuffSuccess: OnStuffSuccess
StuffSentOut --> StuffFailed: OnError
StuffSuccess
```