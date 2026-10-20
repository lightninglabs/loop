# Loop Client Release Notes

#### New Features

* Add experimental same-group asset deposits and Loop In through `loopd`,
  `loop static asset`, and typed fields on existing static-address RPCs.
  The normal deposit/Loop In managers and SQL stores own admission and
  recovery. Server-controlled execution requires locally verified asset
  payments; reservations remain protected until confirmed proof resolution.

* Add Loop-owned deposit/HTLC contracts and an SDK adapter using normal LND
  signing and tapd proof publication. Normal SQL records preserve completed
  fallbacks; interrupted signing attempts use fresh LND sessions.

#### Breaking Changes

#### Bug Fixes

* Recover asset spends through LND chain notifications and verified tapd
  proofs, including original CSV refunds after an extended outage.

#### Maintenance

#### Contributors (Alphabetical Order)
