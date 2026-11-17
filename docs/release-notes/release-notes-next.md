# Loop Client Release Notes

#### New Features

* `SuggestSwaps` (`loop suggestswaps`) now works as a dry run for easy
  autoloop. When easy autoloop is enabled it returns the Loop Out that easy
  autoloop would make next, or sets the new `easy_autoloop_reason` field to the
  reason that it would not make one. Two reasons were added for it:
  `AUTO_REASON_BELOW_MINIMUM` and `AUTO_REASON_NO_ELIGIBLE_CHANNEL`. Before,
  the call ignored easy autoloop and failed when no rules were set.

* With easy autoloop enabled, `SuggestSwaps` lists the channels that easy
  autoloop skipped in `disqualified`, with the reason for each. Two reasons
  were added for it: `AUTO_REASON_CHANNEL_INACTIVE` and
  `AUTO_REASON_PEER_EXCLUDED`. A budget that is not spent yet, but that the
  worst-case fees held for swaps in flight fill, is reported as
  `AUTO_REASON_BUDGET_INSUFFICIENT` rather than `AUTO_REASON_BUDGET_ELAPSED`,
  since there may be room again once those swaps finish.

#### Breaking Changes

* Easy autoloop now respects the client's minimum and maximum swap amounts
  (`loop setparams --minamt/--maxamt`). Before, it only used the server's
  limits. A node with a minimum above the server's now waits for a larger
  excess over its target, and makes fewer, larger swaps. As with autoloop
  rules, easy autoloop stops with an error while these amounts are outside
  the server's limits.

* Easy autoloop no longer dispatches a Loop Out that could cost more than what
  is left of the autoloop fee budget. Before, it only checked that some budget
  was left, so a single swap could go over the budget. The cost is taken from
  the swap's fee limit, without the allowance for edge-case miner fees.

#### Bug Fixes

* A static Loop In that releases its deposits a second time after its payment
  deadline, at its HTLC expiry or when the server rejects the swap, now only
  releases deposits that are still looping in for it. Before, it could move a
  deposit that another Loop In, a withdrawal or a channel open had taken in the
  meantime back to `Deposited`, and retried forever once the deposit was spent.
  A static Loop In also no longer locks a deposit that another Loop In is
  already using, and a failed lock no longer resets deposits that a withdrawal
  or a channel open has taken.

* The static address Loop In timeout sweep now spends the exact HTLC output
  confirmed on chain, including server-published backup fee variants.
  Previously it always targeted the standard fee variant, so a confirmed high
  fee or extremely high fee HTLC transaction could not be swept back after the
  HTLC timed out. Sweeps that would leave a dust output are no longer signed.

#### Maintenance

#### Contributors (Alphabetical Order)
