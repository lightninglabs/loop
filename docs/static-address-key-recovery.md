# Static address key recovery

Loop stores the key family and index for every static address. Restoring lnd
from the same seed while retaining a newer Loop database can leave lnd's key
counters behind those records.

Before activating persisted addresses, startup reads lnd's wallet accounts and
compares each relevant account's external key count (its next index) with the
highest stored index plus one. The legacy/root key uses family 42060; receive
keys use family 42061; change keys use 42062. Static loop-in HTLC keys also use
family 42060, so its highest stored index also covers every persisted static
loop-in HTLC key. Gaps in stored indices do not reduce the required counter.

* Equal counters require no derivation.
* An lnd counter ahead of Loop is valid and remains unchanged. This can result
  from an interrupted issuance after key derivation but before persistence.
  Loop does not generate addresses to fill these gaps.
* A counter behind Loop is advanced using `DeriveNextKey`, after checking that
  explicit derivation of the family's highest stored address key matches its
  stored public key. For family 42060 this is the legacy/root address key.
  A missing account after a successful listing is treated as starting at zero.

Reconciliation does not insert or update Loop address records, broadcast a
transaction, or spend deposits. Each consumed key index is durable in lnd, so
an interrupted startup resumes from the updated counter. Account lookup,
identity verification and derivation errors abort startup rather than exposing
addresses whose counters have not been reconciled.

The address manager's startup wait has no fixed total timeout: a large restore
can require many RPCs. Individual wallet RPC deadlines still apply, and daemon
shutdown or manager errors interrupt the wait. Deposit and swap managers start
after the address manager finishes; the wallet view is never declared ready
partway through reconciliation.

This repairs key allocation only. It does not replace wallet watch restoration
or on-chain scanning, and it cannot repair a wallet restored from another seed.

When reconciliation is needed, the client log (`SADDR`, info level) records the
family, starting counter, and target. Progress is logged every 100 derivations
or after five seconds on a successful derivation, with the remaining count and
percentage, followed by a completion message. Normal synchronized restarts do
not emit recovery progress messages.

Wallet-watch restoration also logs at info level under `SADDR`: before listing
wallet watches, when the first missing script is found, every 100 checked
addresses or after five seconds on a completed address, and when all watches
are ready. Progress reports checked addresses, restored watches, and elapsed
time. It does not report a percentage because addresses are loaded in pages
without an extra count query. A stalled RPC remains subject to its deadline.
