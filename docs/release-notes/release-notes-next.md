# Loop Client Release Notes

#### New Features

* Add experimental same-group asset deposits and Loop In through `loopd`,
  `loop static asset`, and typed fields on existing static-address RPCs.
  The normal deposit/Loop In managers and SQL stores own admission and
  recovery. Server-controlled execution requires locally verified asset
  payments; reservations remain protected until confirmed proof resolution.

* Add Loop-owned deposit/HTLC contracts and native SDK transfer operations in
  the ordinary FSM actions. Existing LND services sign, tapd publishes proofs,
  and normal SQL records preserve completed fallbacks. Interrupted signing
  attempts use fresh LND sessions.

#### Breaking Changes

#### Bug Fixes

* Avoid a deposit recovery lock inversion when reservation and startup
  recovery run concurrently. Unchanged recovery states need no store write.

* Recover asset spends through LND chain notifications and verified tapd
  proofs, including original CSV refunds after an extended outage.

* Preserve native SDK key descriptors when registering OP_TRUE outputs so
  recovered assets remain spendable through the ordinary wallet.

#### Maintenance

* Reuse native SDK packages, existing LND payment clients and the shared
  static-address MuSig2 helper. Asset policy/proof helpers hold no services or
  lifecycle, and normal swap records avoid duplicate invoice/fallback fields.

#### Contributors (Alphabetical Order)
